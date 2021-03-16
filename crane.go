package main

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/net/publicsuffix"
)

const MAX_SIZE int64 = 50000000 // max incoming HTTP request body size (50MB)

var (
	client      *http.Client
	scihubURL   string
	host        string
	port        uint64
	user        string
	pass        string
	buildPrefix string
	templateDir string
)

type Contributor struct {
	FirstName string `xml:"given_name"`
	LastName  string `xml:"surname"`
	Role      string `xml:"contributor_role,attr"`
	Sequence  string `xml:"sequence,attr"`
}

type Meta struct {
	XMLName      xml.Name      `xml:"doi_records"`
	Journal      string        `xml:"doi_record>crossref>journal>journal_metadata>full_title"`
	ISSN         string        `xml:"doi_record>crossref>journal>journal_metadata>issn"`
	Title        string        `xml:"doi_record>crossref>journal>journal_article>titles>title"`
	Contributors []Contributor `xml:"doi_record>crossref>journal>journal_article>contributors>person_name"`
	PubYear      string        `xml:"doi_record>crossref>journal>journal_article>publication_date>year"`
	PubMonth     string        `xml:"doi_record>crossref>journal>journal_article>publication_date>month"`
	FirstPage    string        `xml:"doi_record>crossref>journal>journal_article>pages>first_page"`
	LastPage     string        `xml:"doi_record>crossref>journal>journal_article>pages>last_page"`
	DOI          string        `xml:"doi_record>crossref>journal>journal_article>doi_data>doi"`
	ArxivID      string        `xml:"doi_record>crossref>journal>journal_article>arxiv_data>arxiv_id"`
	Resource     string        `xml:"doi_record>crossref>journal>journal_article>doi_data>resource"`
}

type Paper struct {
	Meta      Meta
	MetaPath  string
	PaperName string
	PaperPath string
}

type Papers struct {
	sync.RWMutex
	List map[string]map[string]*Paper
	Path string
}

type Resp struct {
	Papers           Papers
	Status           string
	LastPaperDL      string
	LastUsedCategory string
}

// getPaperFileNameFromMeta returns the built filename (absent an extension)
// from metadata, consisting of the lowercase last name of the first author
// followed by the year of publication (e.g. doe2020)
func getPaperFileNameFromMeta(p *Meta) string {
	var mainAuthor string
	for _, contributor := range p.Contributors {
		if contributor.Sequence == "first" {
			mainAuthor = strings.Replace(contributor.LastName, "..", "", -1)
			mainAuthor = strings.Replace(contributor.LastName, "/", "", -1)
			break
		}
	}
	if mainAuthor == "" || p.PubYear == "" {
		return ""
	}
	pubYear := strings.Replace(p.PubYear, "..", "", -1)
	pubYear = strings.Replace(p.PubYear, "/", "", -1)
	return fmt.Sprint(strings.ToLower(mainAuthor), pubYear)
}

// getPaperFileNameFromResp returns the name of the file present at resp taken
// first from content-disposition (if exists) then its destination URL
// following redirects; e.g. doe2020
func getPaperFileNameFromResp(resp *http.Response) string {
	var filename string
	if disp, ok := resp.Header["Content-Disposition"]; ok {
		_, params, _ := mime.ParseMediaType(disp[0])
		if f, ok := params["filename"]; ok && f != "" {
			filename = f
		}
	}
	if filename == "" {
		u, _ := url.Parse(resp.Request.URL.String())
		filename = strings.TrimSuffix(filepath.Base(u.Path), "/")
	}
	filename = strings.TrimSuffix(filename, ".pdf")
	filename = strings.Replace(filename, "..", "", -1)
	filename = strings.Replace(filename, "/", "", -1)
	return filename
}

// getUniqueName ensures a paper name is unique, appending "-$ext" until
// a unique name is found and returned
func (papers *Papers) getUniqueName(category string, name string) string {
	newName := name
	ext := 2
	papers.RLock()
	for {
		key := filepath.Join(category, newName+".pdf")
		if _, exists := papers.List[category][key]; exists != true {
			break
		} else {
			newName = fmt.Sprint(name, "-", ext)
			ext++
		}
	}
	papers.RUnlock()
	return newName
}

// findPapersWalk is a WalkFunc passed to filepath.Walk() to process papers
// stored on the filesystem
func (papers *Papers) findPapersWalk(path string, info os.FileInfo,
	err error) error {
	// skip the papers.Path root directory
	if p, _ := filepath.Abs(path); p == papers.Path {
		return nil
	}

	papers.Lock()
	defer papers.Unlock()

	// derive category name (e.g. Mathematics) from directory name; used as key
	var category string
	if i, _ := os.Stat(path); i.IsDir() {
		category = strings.TrimPrefix(path, papers.Path+"/")
	} else {
		category = strings.TrimPrefix(filepath.Dir(path), papers.Path+"/")
	}
	if _, exists := papers.List[category]; exists == false {
		papers.List[category] = make(map[string]*Paper)
	}

	// now that category was added, ensure file is actually a PDF
	if filepath.Ext(path) != ".pdf" {
		return nil
	}

	var paper Paper
	paper.PaperName = strings.TrimSuffix(filepath.Base(path),
		filepath.Ext(path))
	paper.PaperPath = filepath.Join(papers.Path, filepath.Join(category,
		paper.PaperName+".pdf"))

	// XML metadata is not required but highly recommended; PDFs aren't parsed
	// so its our source only source of metadata at the moment
	//
	// PDF parsing looks (and probably is) fairly annoying to support and might
	// be better handled by an external script
	metaPath := filepath.Join(papers.Path, filepath.Join(category,
		paper.PaperName+".meta.xml"))
	if _, err := os.Stat(metaPath); err == nil {
		paper.MetaPath = metaPath

		f, err := os.Open(paper.MetaPath)
		if err != nil {
			return err
		}

		// memory-efficient relative to ioutil.ReadAll()
		r := bufio.NewReader(f)
		d := xml.NewDecoder(r)

		// populate p struct with values derived from doi.org metadata
		if err := d.Decode(&paper.Meta); err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}

	// finally add paper to papers.List set; the subkey is the paper path
	// relative to papers.Path, e.g. Mathematics/example2020.pdf
	relPath := filepath.Join(category, paper.PaperName+".pdf")
	papers.List[category][relPath] = &paper
	return nil
}

// PopulatePapers wraps filepath.Walk() and populates the papers set with
// discovered papers
func (papers *Papers) PopulatePapers() error {
	if err := filepath.Walk(papers.Path, papers.findPapersWalk); err != nil {
		return err
	}
	return nil
}

// NewPaperFromDOI contains routines used to retrieve papers from remote
// endpoints provided a DOI
func (papers *Papers) NewPaperFromDOI(doi []byte, category string) (*Paper,
	error) {
	var paper Paper

	meta, err := getMetaFromDOI(client, doi)
	if err != nil {
		return nil, err
	}

	// create a temporary file to store XML stream
	tmpXML, err := ioutil.TempFile("", "tmp-*.meta.xml")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpXML.Name())

	e := xml.NewEncoder(tmpXML)
	err = e.Encode(meta)
	if err != nil {
		return nil, err
	}
	tmpXML.Close()

	name := getPaperFileNameFromMeta(meta) // doe2020
	if name == "" {
		// last-resort condition if metadata lacking author or publication year
		name = strings.Replace(string(doi), "..", "", -1)
		name = strings.Replace(string(doi), "/", "", -1)
	}

	// doe2020-(2, 3, 4...) if n already exists in set
	uniqueName := papers.getUniqueName(category, name)

	// if not matching, check if DOIs match (genuine duplicate)
	if name != uniqueName {
		key := filepath.Join(category, name+".pdf")
		papers.RLock()
		if meta.DOI == papers.List[category][key].Meta.DOI {
			return nil, fmt.Errorf("paper %q with DOI %q already downloaded",
				name, string(doi))
		}
		papers.RUnlock()
	}

	paper.PaperName = uniqueName
	paper.PaperPath = filepath.Join(filepath.Join(papers.Path, category),
		paper.PaperName+".pdf")
	paper.MetaPath = filepath.Join(filepath.Join(papers.Path, category),
		paper.PaperName+".meta.xml")

	// make outbound request to sci-hub, save paper to temporary location
	url := scihubURL + string(doi)
	tmpPDF, err := getPaper(client, url)
	defer os.Remove(tmpPDF)
	if err != nil {
		// try passing resource URL (from doi.org metadata) to sci-hub instead
		// (force cache)
		if meta.Resource != "" {
			url = scihubURL + meta.Resource
			tmpPDF, err = getPaper(client, url)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	if err := renameFile(tmpPDF, paper.PaperPath); err != nil {
		return nil, err
	}
	if err := renameFile(tmpXML.Name(), paper.MetaPath); err != nil {
		return nil, err
	}
	paper.Meta = *meta

	papers.Lock()
	papers.List[category][filepath.Join(category,
		paper.PaperName+".pdf")] = &paper
	papers.Unlock()
	return &paper, nil
}

// NewPaperFromDirectLink contains routines used to retrieve papers from remote
// endpoints provided a direct link's http.Response and/or optional metadata
func (papers *Papers) NewPaperFromDirectLink(resp *http.Response, meta *Meta,
	category string) (*Paper, error) {
	tmpPDF, err := ioutil.TempFile("", "tmp-*.pdf")
	if err != nil {
		return &Paper{}, err
	}
	err = saveRespBody(resp, tmpPDF.Name())
	if err != nil {
		return &Paper{}, err
	}
	if err := tmpPDF.Close(); err != nil {
		return &Paper{}, err
	}
	defer os.Remove(tmpPDF.Name())

	var paper Paper
	paper.PaperName = papers.getUniqueName(category,
		getPaperFileNameFromMeta(meta))
	if paper.PaperName == "" {
		paper.PaperName = papers.getUniqueName(category,
			getPaperFileNameFromResp(resp))
	}

	if err != nil {
		return &Paper{}, err
	}
	paper.PaperPath = filepath.Join(papers.Path,
		filepath.Join(category, paper.PaperName+".pdf"))

	if err := renameFile(tmpPDF.Name(), paper.PaperPath); err != nil {
		return nil, err
	}
	papers.Lock()
	papers.List[category][filepath.Join(category,
		paper.PaperName+".pdf")] = &paper
	papers.Unlock()
	return &paper, nil
}

// DeletePaper deletes a paper and its metadata from the filesystem and the
// papers.List set
func (papers *Papers) DeletePaper(paper string) error {
	// check if the category in which the paper is said to belong
	// exists
	papers.RLock()
	category := filepath.Dir(paper)
	if _, exists := papers.List[category]; exists != true {
		return fmt.Errorf("category %q does not exist\n",
			papers.List[filepath.Dir(paper)])
	}

	// check if paper already exists in the provided category
	if _, exists := papers.List[category][paper]; exists != true {
		return fmt.Errorf("paper %q does not exist in category %q\n", paper,
			category)
	}
	papers.RUnlock()

	// paper and category exists and the paper belongs to the provided
	// category; remove it and its XML metadata
	papers.Lock()
	if err := os.Remove(papers.List[category][paper].PaperPath); err != nil {
		return err
	}

	// XML metadata optional; delete it if it exists
	metaPath := papers.List[category][paper].MetaPath
	if metaPath != "" {
		if _, err := os.Stat(metaPath); err == nil {
			if err := os.Remove(metaPath); err != nil {
				return err
			}
		}
	}
	delete(papers.List[category], paper)
	papers.Unlock()
	return nil
}

// DeleteCategory deletes a category and its contents from the filesystem and
// the papers.List set
func (papers *Papers) DeleteCategory(category string) error {
	papers.RLock()
	if _, exists := papers.List[category]; exists != true {
		return fmt.Errorf("category %q does not exist in the set\n", category)
	}
	papers.RUnlock()

	papers.Lock()
	if err := os.RemoveAll(filepath.Join(papers.Path, category)); err != nil {
		return err
	}

	// remove subcategories (nested directories) which exist under the primary
	for key, _ := range papers.List {
		if strings.HasPrefix(key, category+"/") {
			delete(papers.List, key)
		}
	}

	delete(papers.List, category)
	papers.Unlock()
	return nil
}

// MovePaper moves a paper to the destination category on the filesystem and
// the papers.List set
func (papers *Papers) MovePaper(paper string, category string) error {
	papers.RLock()
	prevCategory := filepath.Dir(paper)
	if _, exists := papers.List[prevCategory]; exists != true {
		return fmt.Errorf("category %q does not exist\n", prevCategory)
	}
	if _, exists := papers.List[category]; exists != true {
		return fmt.Errorf("category %q does not exist\n", category)
	}
	if _, exists := papers.List[prevCategory][paper]; exists != true {
		return fmt.Errorf("paper %q does not exist in category %q\n", paper,
			prevCategory)
	}
	if _, exists := papers.List[category][paper]; exists == true {
		return fmt.Errorf("paper %q exists in destination category %q\n",
			paper, category)
	}
	papers.RUnlock()

	papers.Lock()
	paperDest := filepath.Join(filepath.Join(papers.Path, category),
		papers.List[prevCategory][paper].PaperName+".pdf")
	if err := os.Rename(papers.List[prevCategory][paper].PaperPath, paperDest);
		err != nil {
		return err
	}

	papers.List[category][filepath.Join(category,
		filepath.Base(paper))] = papers.List[prevCategory][paper]

	papers.List[category][filepath.Join(category,
		filepath.Base(paper))].PaperPath = paperDest

	// XML metadata optional; move if any exists
	metaPath := papers.List[prevCategory][paper].MetaPath
	if metaPath != "" {
		if _, err := os.Stat(metaPath); err == nil {
			metaName := papers.List[prevCategory][paper].PaperName +
				".meta.xml"
			metaDest := filepath.Join(filepath.Join(papers.Path, category),
				metaName)
			if err := os.Rename(metaPath, metaDest); err != nil {
				return err
			}
			papers.List[category][filepath.Join(category,
				filepath.Base(paper))].MetaPath = metaDest
		}
	}
	delete(papers.List[prevCategory], paper)

	papers.Unlock()
	return nil
}

// RenameCategory renames a category on the filesystem and the paper.List set
func (papers *Papers) RenameCategory(oldCategory string,
	newCategory string) error {
	papers.RLock()
	if _, exists := papers.List[oldCategory]; exists != true {
		return fmt.Errorf("category %q does not exist in the set\n", oldCategory)
	}
	if _, exists := papers.List[newCategory]; exists == true {
		return fmt.Errorf("category %q already exists in the set\n", newCategory)
	}
	if err := os.Rename(filepath.Join(papers.Path, oldCategory),
		filepath.Join(papers.Path, newCategory)); err != nil {
		return err
	}
	papers.RUnlock()

	papers.Lock()
	papers.List[newCategory] = make(map[string]*Paper)
	for k, v := range papers.List[oldCategory] {
		pPaperPath := filepath.Join(papers.Path, filepath.Join(newCategory,
			v.PaperName+".pdf"))
		pK := filepath.Join(newCategory, filepath.Base(k))
		papers.List[newCategory][pK] = papers.List[oldCategory][k]
		papers.List[newCategory][pK].PaperPath = pPaperPath

		if v.MetaPath != "" {
			pMetaPath := filepath.Join(papers.Path, filepath.Join(newCategory,
				v.PaperName+".meta.xml"))
			papers.List[newCategory][pK].MetaPath = pMetaPath
		}
	}
	delete(papers.List, oldCategory)

	papers.Unlock()
	return nil
}

// ProcessAddPaperInput processes takes user input and attempts to retrieve
// a DOI and initiate paper download
func (papers *Papers) ProcessAddPaperInput(category string,
	input string) (*Paper, error) {
	if strings.HasPrefix(input, "http") {
		resp, err := makeRequest(client, input)
		if err != nil {
			return &Paper{}, err
		}
		if resp.Header.Get("Content-Type") == "application/pdf" {
			paper, err := papers.NewPaperFromDirectLink(resp, &Meta{}, category)
			if err != nil {
				return &Paper{}, err
			}
			return paper, nil
		}

		meta, err := getMetaFromCitation(resp)
		if err != nil {
			return nil, err
		}
		if meta.Resource != "" {
			resp, err := makeRequest(client, meta.Resource)
			if err == nil && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/pdf") {
				paper, err := papers.NewPaperFromDirectLink(resp, meta, category)
				if err != nil {
					return nil, err
				} else {
					tmpXML, err := ioutil.TempFile("", "tmp-*.meta.xml")
					if err != nil {
						return nil, err
					}
					defer os.Remove(tmpXML.Name())

					e := xml.NewEncoder(tmpXML)
					err = e.Encode(meta)
					if err != nil {
						return nil, err
					}
					tmpXML.Close()

					paper.MetaPath = filepath.Join(filepath.Join(papers.Path,
						category), paper.PaperName+".meta.xml")
					if err := renameFile(tmpXML.Name(), paper.MetaPath); err != nil {
						return nil, err
					}

					paper.Meta = *meta
					return paper, nil
				}
			}
		}
		if meta.DOI != "" {
			paper, err := papers.NewPaperFromDOI([]byte(meta.DOI), category)
			if err != nil {
				return nil, err
			}
			return paper, nil
		} else {
			return &Paper{}, fmt.Errorf("%q: DOI could not be discovered", input)
		}
	} else {
		doi := getDOIFromBytes([]byte(input))
		if doi == nil {
			return &Paper{}, fmt.Errorf("%q is not a valid DOI or URL\n", input)
		}
		if paper, err := papers.NewPaperFromDOI(doi, category); err != nil {
			return nil, fmt.Errorf("%q: %v", input, err)
		} else {
			return paper, nil
		}
	}
}

func main() {
	// some publishers have cookie + HTTP 302 checks (e.g. sagepub), let's look
	// like a real browser
	options := cookiejar.Options{
		PublicSuffixList: publicsuffix.List,
	}
	cookies, err := cookiejar.New(&options)
	if err != nil {
		panic(err)
	}

	// custom DialContext which blocks outbound requests to local addresses and
	// interfaces (security)
	http.DefaultTransport.(*http.Transport).DialContext = func(ctx context.Context,
		network, addr string) (net.Conn, error) {
		// we could run our check after a dial, but we'd have to discard
		// connect errors to prevent exposure of local services; a preemptive
		// lookup is the lesser of two evils, I think
		hosts, _ := net.LookupHost(addr[:strings.LastIndex(addr, ":")])
		for _, host := range hosts {
			if isPrivateIP(net.ParseIP(host)) {
				return nil, errors.New("requests to private IPs are blocked")
			}
		}
		conn, err := net.Dial(network, addr)
		if err != nil {
			return nil, err
		}
		return conn, err
	}
	client = &http.Client{Jar: cookies}

	var papers Papers
	papers.List = make(map[string]map[string]*Paper)

	flag.StringVar(&scihubURL, "sci-hub", "https://sci-hub.se/", "Sci-Hub URL")
	flag.StringVar(&papers.Path, "path", "./papers",
		"Absolute or relative path to papers folder")
	flag.StringVar(&host, "host", "127.0.0.1", "IP address to listen on")
	flag.Uint64Var(&port, "port", 9090, "Port to listen on")
	flag.StringVar(&user, "user", "", "Username for /admin/ endpoints (optional)")
	flag.StringVar(&pass, "pass", "", "Password for /admin/ endpoints (optional)")
	flag.Parse()

	papers.Path, _ = filepath.Abs(papers.Path)

	if !strings.HasSuffix(scihubURL, "/") {
		scihubURL = scihubURL + "/"
	}
	if _, err := os.Stat(papers.Path); os.IsNotExist(err) {
		os.Mkdir(papers.Path, os.ModePerm)
	}
	if err := papers.PopulatePapers(); err != nil {
		panic(err)
	}
	if net.ParseIP(host) == nil {
		panic(errors.New("Host flag could not be parsed; is it an IP address?"))
	}

	// prefer system-installed template assets over project-local paths
	if _, err := os.Stat(filepath.Join(buildPrefix,
		"/share/crane/templates")); err != nil {
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Fatal(err)
		}
		templateDir = filepath.Join(dir, "templates")
	} else {
		templateDir = filepath.Join(buildPrefix, "/share/crane/templates")
	}

	http.HandleFunc("/", papers.IndexHandler)
	http.HandleFunc("/admin/", papers.AdminHandler)
	http.HandleFunc("/admin/edit/", papers.EditHandler)
	http.HandleFunc("/admin/add/", papers.AddHandler)
	http.HandleFunc("/download/", papers.DownloadHandler)
	fmt.Printf("Listening on %v port %v (http://%v:%v/)\n", host, port, host,
		port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", host, port), nil))
}
