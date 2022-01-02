package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var templateDir = getTemplateDir()

var indexTemp = template.Must(template.ParseFiles(
	filepath.Join(templateDir, "layout.html"),
	filepath.Join(templateDir, "index.html"),
	filepath.Join(templateDir, "list.html"),
))
var adminTemp = template.Must(template.ParseFiles(
	filepath.Join(templateDir, "admin.html"),
	filepath.Join(templateDir, "layout.html"),
	filepath.Join(templateDir, "list.html"),
))
var editTemp = template.Must(template.ParseFiles(
	filepath.Join(templateDir, "admin-edit.html"),
	filepath.Join(templateDir, "layout.html"),
	filepath.Join(templateDir, "list.html"),
))

func cat(cat string) string {

	return strings.Replace(cat, "-", "&#8209;", -1)
}

// getTemplateDir returns the absolute path of the templates directory,
// preferring system-installed assets over the project-local path
func getTemplateDir() string {

	if _, err := os.Stat(filepath.Join(buildPrefix,
		"/share/crane/templates")); err != nil {
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			log.Fatal(err)
		}
		return filepath.Join(dir, "templates")
	} else {
		return filepath.Join(buildPrefix, "/share/crane/templates")
	}
}

// IndexHandler renders the index of papers stored in papers.Path
func (papers *Papers) IndexHandler(w http.ResponseWriter, r *http.Request) {

	// catch-all for paths unhandled by direct http.HandleFunc calls
	if r.URL.Path != "/" {
		http.Error(w, http.StatusText(http.StatusNotFound), http.StatusNotFound)
		return
	}
	res := Resp{Papers: *papers}
	indexTemp.Execute(w, &res)
}

// AdminHandler renders the index of papers stored in papers.Path with
// additional forms to modify the collection (add, delete, rename...)
func (papers *Papers) AdminHandler(w http.ResponseWriter, r *http.Request) {

	res := Resp{Papers: *papers}
	if user != "" && pass != "" {
		username, password, ok := r.BasicAuth()
		if ok && user == username && pass == password {
			adminTemp.Execute(w, &res)
		} else {
			w.Header().Add("WWW-Authenticate",
				`Basic realm="Please authenticate"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized),
				http.StatusUnauthorized)
		}
	} else {
		adminTemp.Execute(w, &res)
	}
}

// EditHandler renders the index of papers stored in papers.Path, prefixing
// a checkbox to each unique paper and category for modification
func (papers *Papers) EditHandler(w http.ResponseWriter, r *http.Request) {

	res := Resp{Papers: *papers}
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
		editTemp.Execute(w, &res)
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
	editTemp.Execute(w, &res)
}

// AddHandler provides support for new paper processing and category addition
func (papers *Papers) AddHandler(w http.ResponseWriter, r *http.Request) {

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
	res := Resp{}

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
	res.Papers = *papers
	adminTemp.Execute(w, &res)
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
