package main

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

var privateIPBlocks []*net.IPNet

// isPrivateIP checks to if the provided IP address is a loopback, link-local
// or unique-local address
//
// credit: https://stackoverflow.com/a/50825191
func isPrivateIP(ip net.IP) bool {
	if privateIPBlocks == nil {
		for _, cidr := range []string{
			"127.0.0.0/8",    // IPv4 loopback
			"10.0.0.0/8",     // RFC1918
			"172.16.0.0/12",  // RFC1918
			"192.168.0.0/16", // RFC1918
			"169.254.0.0/16", // RFC3927 link-local
			"::1/128",        // IPv6 loopback
			"fe80::/10",      // IPv6 link-local
			"fc00::/7",       // IPv6 unique local addr
		} {
			_, block, err := net.ParseCIDR(cidr)
			if err != nil {
				panic(fmt.Errorf("parse error on %q: %v", cidr, err))
			}
			privateIPBlocks = append(privateIPBlocks, block)
		}
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	for _, block := range privateIPBlocks {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// getDOIFromBytes returns the DOI parsed from the provided []byte slice
func getDOIFromBytes(b []byte) []byte {
	re := regexp.MustCompile(`(10[.][0-9]{4,}[^\s"/<>]*/[^\s"'<>,\{\};\[\]\?&]+)`)
	return re.Find(b)
}

// makeRequest makes a request to a remote resource using the provided
// *http.Client and returns its *http.Response
func makeRequest(client *http.Client, u string) (*http.Response, error) {
	req, err := http.NewRequest("GET", u, nil)

	// sciencedirect and company block atypical user agents
	req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; rv:78.0) Gecko/20100101 Firefox/78.0")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%q: status code not OK", u)
	}
	return resp, nil
}

// getMetaFromCitation parses an *http.Response for <meta> tags to populate a
// paper's Meta attributes and returns the paper
func getMetaFromCitation(resp *http.Response) (*Meta, error) {
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, err
	}

	var meta Meta
	var f func(*html.Node)
	f = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			var name string
			var cont string
			for _, a := range n.Attr {
				if a.Key == "name" || a.Key == "property" {
					name = a.Val
				} else if a.Key == "content" {
					cont = a.Val
				}
			}
			switch name {
			case "citation_title":
				meta.Title = cont
			case "citation_author":
				var c Contributor
				// Doe, Jain
				if strings.Contains(cont, ",") {
					v := strings.Split(cont, ", ")
					c.FirstName = strings.Join(v[1:], " ")
					c.LastName = v[0]
					// Jain Doe
				} else {
					v := strings.Split(cont, " ")
					c.FirstName = strings.Join(v[:len(v)-1], " ")
					c.LastName = strings.Join(v[len(v)-1:], " ")
				}
				c.Role = "author"
				if len(meta.Contributors) > 0 {
					c.Sequence = "additional"
				} else {
					c.Sequence = "first"
				}
				meta.Contributors = append(meta.Contributors, c)
			case "citation_date", "citation_publication_date":
				var formats = []string{"2006-01-02", "2006/01/02", "2006"}
				for _, format := range formats {
					t, err := time.Parse(format, cont)
					if err == nil {
						meta.PubMonth = t.Month().String()
						meta.PubYear = strconv.Itoa(t.Year())
						break
					}
				}
			case "citation_journal_title", "og:site_name", "DC.Publisher":
				meta.Journal = cont
			case "citation_firstpage":
				meta.FirstPage = cont
			case "citation_lastpage":
				meta.LastPage = cont
			case "citation_doi":
				meta.DOI = cont
			case "citation_arxiv_id":
				meta.ArxivID = cont
			case "citation_pdf_url":
				meta.Resource = cont
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			f(c)
		}
	}
	f(doc)
	return &meta, nil
}

// renameFile is an alternative to os.Rename which supports moving files
// between devices where os.Rename would return an error (cross-device link)
func renameFile(src string, dst string) (err error) {
	if src == dst {
		return nil
	}
	err = copyFile(src, dst)
	if err != nil {
		return fmt.Errorf("failed to copy source file %s to %s: %s", src, dst, err)
	}
	err = os.RemoveAll(src)
	if err != nil {
		return fmt.Errorf("failed to cleanup source file %s: %s", src, err)
	}
	return nil
}

// copyFile copies a file located at src to dst, used by renameFile()
//
// credit: https://gist.github.com/r0l1/92462b38df26839a3ca324697c8cba04
func copyFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer func() {
		if e := out.Close(); e != nil {
			err = e
		}
	}()

	_, err = io.Copy(out, in)
	if err != nil {
		return
	}

	err = out.Sync()
	if err != nil {
		return
	}

	si, err := os.Stat(src)
	if err != nil {
		return
	}
	err = os.Chmod(dst, si.Mode())
	if err != nil {
		return
	}

	return
}

// getMetaFromDOI saves doi.org API data to TempFile and returns its path
func getMetaFromDOI(client *http.Client, doi []byte) (*Meta, error) {
	u := "https://doi.org/" + string(doi)
	req, err := http.NewRequest("GET", u, nil)

	req.Header.Add("Accept", "application/vnd.crossref.unixref+xml;q=1,application/rdf+xml;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%q: failed to get metadata", u)
	}
	if resp.Header.Get("Content-Type") != "application/vnd.crossref.unixref+xml" {
		return nil, fmt.Errorf("%q: content-type not application/vnd.crossref.unixref+xml", u)
	}
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(resp.Body)
	d := xml.NewDecoder(r)

	// populate p struct with values derived from doi.org metadata
	var meta Meta
	if err := d.Decode(&meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// getPaper saves makes an outbound request to a remote resource and saves the
// response body to a temporary file, returning its path, provided the response
// has the content-type application/pdf
func getPaper(client *http.Client, u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)

	// sci-hub gives us the paper directly (no iframe) if we're on mobile
	req.Header.Add("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 13_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.1.2 Mobile/15E148 Safari/604.1")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%q: status code not OK", u)
	}
	if resp.Header.Get("Content-Type") != "application/pdf" {
		return "", fmt.Errorf("%q: content-type not application/pdf", u)
	}
	tmpPDF, err := ioutil.TempFile("", "tmp-*.pdf")
	if err != nil {
		return "", err
	}

	// write resp.Body (paper data) to tmpPDF
	if err := saveRespBody(resp, tmpPDF.Name()); err != nil {
		return "", err
	}
	if err := tmpPDF.Close(); err != nil {
		return "", err
	}
	return tmpPDF.Name(), nil
}

// saveRespBody writes the provided http.Response to path
func saveRespBody(resp *http.Response, path string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()

	r := http.MaxBytesReader(nil, resp.Body, MAX_SIZE)
	_, err = io.Copy(out, r)
	if err != nil {
		return err
	}
	return nil
}
