package main

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html/template"
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
	Resource     string        `xml:"doi_record>crossref>journal>journal_article>doi_data>resource"`
}

type Paper struct {
	Meta      Meta
	MetaPath  string
	PaperName string
	PaperPath string
}

type Papers struct {
	List map[string]map[string]*Paper
	Path string
}

type Resp struct {
	Papers           map[string]map[string]*Paper
	Status           string
	LastPaperDL      string
	LastUsedCategory string
}

// getPaperFileNameFromMeta returns the built filename (absent an extension)
// from doi.org metadata, consisting of the lowercase last name of the first
// author followed by the year of publication (e.g. doe2020)
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
	return filename
}

// getUniqueName ensures a paper name is unique, appending "-$ext" until
// a unique name is found and returned
func (papers *Papers) getUniqueName(category string, name string) string {
	ext := 2
	for {
		key := filepath.Join(category, name+".pdf")
		if _, exists := papers.List[category][key]; exists != true {
			break
		} else {
			name = fmt.Sprint(name, "-", ext)
			ext++
		}
	}
	return name
}

// findPapersWalk is a WalkFunc passed to filepath.Walk() to process papers
// stored on the filesystem
func (papers *Papers) findPapersWalk(path string, info os.FileInfo,
	err error) error {
	// skip the papers.Path root directory
	if p, _ := filepath.Abs(path); p == papers.Path {
		return nil
	}

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

// NewPaperFromDirectLink contains routines used to retrieve papers from remote
// endpoints provided a direct link's http.Response
func (papers *Papers) NewPaperFromDirectLink(resp *http.Response,
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
		getPaperFileNameFromResp(resp))

	if err != nil {
		return &Paper{}, err
	}
	paper.PaperPath = filepath.Join(papers.Path,
		filepath.Join(category, paper.PaperName+".pdf"))

	if err := renameFile(tmpPDF.Name(), paper.PaperPath); err != nil {
		return nil, err
	}
	papers.List[category][filepath.Join(category,
		paper.PaperName+".pdf")] = &paper
	return &paper, nil
}

// NewPaperFromDOI contains routines used to retrieve papers from remote
// endpoints provided a DOI
func (papers *Papers) NewPaperFromDOI(doi []byte, category string) (*Paper,
	error) {
	tmpXML, err := getMetaFromDOI(client, doi)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpXML)

	// open temporary XML file for parsing
	f, err := os.Open(tmpXML)
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	d := xml.NewDecoder(r)

	// populate p struct with values derived from doi.org metadata
	var paper Paper
	if err := d.Decode(&paper.Meta); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	name := getPaperFileNameFromMeta(&paper.Meta) // doe2020
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
		if paper.Meta.DOI == papers.List[category][key].Meta.DOI {
			return nil, fmt.Errorf("paper %q with DOI %q already downloaded",
				name, string(doi))
		}
	}

	paper.PaperName = uniqueName
	paper.PaperPath = filepath.Join(filepath.Join(papers.Path, category),
		paper.PaperName+".pdf")
	paper.MetaPath = filepath.Join(filepath.Join(papers.Path, category),
		paper.PaperName+".meta.xml")

	// parse scihubURL and join it w/ the DOI (accounts for no trailing slash)
	url, _ := url.Parse(scihubURL)
	url.Path = filepath.Join(url.Path, string(doi))

	// make outbound request to sci-hub, save paper to temporary location
	tmpPDF, err := getPaper(client, url.String())
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmpPDF)

	if err := renameFile(tmpPDF, paper.PaperPath); err != nil {
		return nil, err
	}
	if err := renameFile(tmpXML, paper.MetaPath); err != nil {
		return nil, err
	}
	papers.List[category][filepath.Join(category,
		paper.PaperName+".pdf")] = &paper
	return &paper, nil
}

// DeletePaper deletes a paper and its metadata from the filesystem and the
// papers.List set
func (papers *Papers) DeletePaper(paper string) error {
	// check if the category in which the paper is said to belong
	// exists
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

	// paper and category exists and the paper belongs to the provided
	// category; remove it and its XML metadata
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
	return nil
}

// DeleteCategory deletes a category and its contents from the filesystem and
// the papers.List set
func (papers *Papers) DeleteCategory(category string) error {
	if _, exists := papers.List[category]; exists != true {
		return fmt.Errorf("category %q does not exist in the set\n", category)
	}
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
	return nil
}

// MovePaper moves a paper to the destination category on the filesystem and
// the papers.List set
func (papers *Papers) MovePaper(paper string, category string) error {
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
	return nil
}

// RenameCategory renames a category on the filesystem and the paper.List set
func (papers *Papers) RenameCategory(oldCategory string,
	newCategory string) error {
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
	return nil
}

// ProcessAddPaperInput processes takes user input and attempts to retrieve
// a DOI and initiate paper download
func (papers *Papers) ProcessAddPaperInput(category string,
	input string) (*Paper, error) {
	var doi []byte

	// URL processing routine; download paper directly or check page for a DOI
	if u, _ := url.Parse(input); u.Scheme != "" && u.Host != "" {
		resp, err := makeRequest(client, input)
		if err != nil {
			return &Paper{}, err
		}
		if resp.Header.Get("Content-Type") == "application/pdf" {
			paper, err := papers.NewPaperFromDirectLink(resp, category)
			if err != nil {
				return &Paper{}, err
			}
			return paper, nil
		}
		doi = getDOIFromPage(resp)

		// last resort, pass url to sci-hub and see if they know the DOI
		if doi == nil {
			resp, err = makeRequest(client, scihubURL+input)
			if err != nil {
				return &Paper{}, err
			}
			doi = getDOIFromPage(resp)
		}
		if doi == nil {
			return &Paper{}, fmt.Errorf("%q: DOI not found on page", input)
		}
	} else {
		// input was not a URL, hopefully it has or contains a DOI
		doi = getDOIFromBytes([]byte(input))
		if doi == nil {
			return &Paper{}, fmt.Errorf("%q is not a valid DOI or URL\n", input)
		}
	}
	paper, err := papers.NewPaperFromDOI(doi, category)
	if err != nil {
		if u, _ := url.Parse(input); u.Scheme != "" && u.Host != "" {
			// try to force sci-hub to cache paper if dl failed and input was
			// URL, retry
			makeRequest(client, scihubURL+input)
			paper, err := papers.NewPaperFromDOI(doi, category)
			if err != nil {
				return &Paper{}, err
			}
			return paper, nil
		} else {
			return &Paper{}, err
		}
	}
	return paper, nil
}

// IndexHandler renders the index of papers stored in papers.Path
func (papers *Papers) IndexHandler(w http.ResponseWriter, r *http.Request) {
	// catch-all for paths unhandled by direct http.HandleFunc calls
	if r.URL.Path != "/" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	t, _ := template.ParseFiles(filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "index.html"),
		filepath.Join(templateDir, "list.html"),
	)
	res := Resp{
		Papers: papers.List,
	}
	t.Execute(w, &res)
}

// AdminHandler renders the index of papers stored in papers.Path with
// additional forms to modify the collection (add, delete, rename...)
func (papers *Papers) AdminHandler(w http.ResponseWriter, r *http.Request) {
	t, _ := template.ParseFiles(filepath.Join(templateDir, "admin.html"),
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "list.html"),
	)
	res := Resp{
		Papers: papers.List,
	}
	if user != "" && pass != "" {
		username, password, ok := r.BasicAuth()
		if ok && user == username && pass == password {
			t.Execute(w, &res)
		} else {
			w.Header().Add("WWW-Authenticate",
				`Basic realm="Please authenticate"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
		}
	} else {
		t.Execute(w, &res)
	}
}

// EditHandler renders the index of papers stored in papers.Path, prefixing
// a checkbox to each unique paper and category for modification
func (papers *Papers) EditHandler(w http.ResponseWriter, r *http.Request) {
	t, _ := template.ParseFiles(filepath.Join(templateDir, "admin-edit.html"),
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "list.html"),
	)
	res := Resp{
		Papers: papers.List,
	}
	if user != "" && pass != "" {
		username, password, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Add("WWW-Authenticate",
				`Basic realm="Please authenticate"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}
	}
	if err := r.ParseForm(); err != nil {
		res.Status = err.Error()
		t.Execute(w, &res)
		return
	}

	if action := r.FormValue("action"); action == "delete" {
		for _, paper := range r.Form["paper"] {
			if res.Status != "" {
				break
			}
			if err := papers.DeletePaper(paper); err != nil {
				res.Status = err.Error()
			}
		}
		for _, category := range r.Form["category"] {
			if res.Status != "" {
				break
			}
			if err := papers.DeleteCategory(category); err != nil {
				res.Status = err.Error()
			}
		}
		if res.Status == "" {
			res.Status = "delete successful"
		}
	} else if strings.HasPrefix(action, "move") {
		destCategory := strings.SplitN(action, "move-", 2)[1]
		for _, paper := range r.Form["paper"] {
			if res.Status != "" {
				break
			}
			if err := papers.MovePaper(paper, destCategory); err != nil {
				res.Status = err.Error()
			}
		}
		if res.Status == "" {
			res.Status = "move successful"
		}
	} else {
		rc := r.FormValue("rename-category")
		rt := r.FormValue("rename-to")
		if rc != "" && rt != "" {
			// ensure filesystem safety of category names
			rc = strings.Trim(strings.Replace(rc, "..", "", -1), "/.")
			rt = strings.Trim(strings.Replace(rt, "..", "", -1), "/.")

			if err := papers.RenameCategory(rc, rt); err != nil {
				res.Status = err.Error()
			}
			if res.Status == "" {
				res.Status = "rename successful"
			}
		}
	}
	t.Execute(w, &res)
}

// AddHandler provides support for new paper processing and category addition
func (papers *Papers) AddHandler(w http.ResponseWriter, r *http.Request) {
	t, _ := template.ParseFiles(filepath.Join(templateDir, "admin.html"),
		filepath.Join(templateDir, "layout.html"),
		filepath.Join(templateDir, "list.html"),
	)
	if user != "" && pass != "" {
		username, password, ok := r.BasicAuth()
		if !ok || user != username || pass != password {
			w.Header().Add("WWW-Authenticate",
				`Basic realm="Please authenticate"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
			return
		}
	}
	p := r.FormValue("dl-paper")
	c := r.FormValue("dl-category")
	nc := r.FormValue("new-category")

	// sanitize input; we use the category to build the path used to save
	// papers
	nc = strings.Trim(strings.Replace(nc, "..", "", -1), "/.")
	res := Resp{Papers: papers.List}

	// paper download, both required fields populated
	if len(strings.TrimSpace(p)) > 0 && len(strings.TrimSpace(c)) > 0 {
		if paper, err := papers.ProcessAddPaperInput(c, p); err != nil {
			res.Status = err.Error()
		} else {
			if paper.Meta.Title != "" {
				res.Status = fmt.Sprintf("%q downloaded successfully",
					paper.Meta.Title)
			} else {
				res.Status = fmt.Sprintf("%q downloaded successfully",
					paper.PaperName)
			}
			res.LastPaperDL = strings.TrimPrefix(paper.PaperPath,
				papers.Path+"/")
		}
		res.LastUsedCategory = c
	} else if len(strings.TrimSpace(nc)) > 0 {
		// accounts for nested category addition; e.g. "foo/bar/baz" where
		// "foo/bar" and/or "foo" do not already exist
		n := nc
		for n != "." {
			_, exists := papers.List[n]
			if exists == true {
				res.Status = fmt.Sprintf("category %q already exists", n)
			} else if err := os.MkdirAll(filepath.Join(papers.Path, n),
				os.ModePerm); err != nil {
				res.Status = fmt.Sprintf(err.Error())
			} else {
				papers.List[n] = make(map[string]*Paper)
			}
			if res.Status != "" {
				break
			}
			res.LastUsedCategory = n
			n = filepath.Dir(n)
		}
		if res.Status == "" {
			res.Status = fmt.Sprintf("category %q added successfully", nc)
		}
	}
	t.Execute(w, &res)
}

// DownloadHandler serves saved papers up for download
func (papers *Papers) DownloadHandler(w http.ResponseWriter, r *http.Request) {
	paper := strings.TrimPrefix(r.URL.Path, "/download/")
	category := filepath.Dir(paper)

	// return 404 if the provided paper category or paper key do not exist in
	// the papers set
	if _, exists := papers.List[category]; exists == false {
		http.Error(w, http.StatusText(http.StatusNotFound),
			http.StatusNotFound)
		return
	}
	if _, exists := papers.List[category][paper]; exists == false {
		http.Error(w, http.StatusText(http.StatusNotFound),
			http.StatusNotFound)
		return
	}

	// ensure the paper (PaperPath) actually exists on the filesystem
	i, err := os.Stat(papers.List[category][paper].PaperPath)
	if os.IsNotExist(err) {
		http.Error(w, http.StatusText(http.StatusNotFound),
			http.StatusNotFound)
	} else if i.IsDir() {
		http.Error(w, http.StatusText(http.StatusForbidden),
			http.StatusForbidden)
	} else {
		http.ServeFile(w, r, papers.List[category][paper].PaperPath)
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
