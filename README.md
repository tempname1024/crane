# Crane

Crane is a literature download and organization web service. Paper and metadata
download is possible for (nearly) every open-access journal which satisfies
citation HTML `<meta>` tags, journals supported by sci-hub, and direct download
links.

Goals of the project are to be minimal, support data portability (no databases
or app-proprietary formats), and secure. Paper "categories" are simply
directories created on the filesystem, and paper metadata is derived from
[doi.org](https://www.doi.org/) or <meta> tags and written to disk as XML.

A screenshot of the admin interface can be [found here](screenshots/admin.png).

## Installation

Crane can be compiled with `make` or `go build`, and installed system-wide by
running `make install` with root-level permissions.

## Usage

Crane can be run locally or on a server. The index (`"/"`) endpoint lists papers
but does not permits modification to the set. The admin (`"/admin/"`) endpoint
supports optional authentication and permits paper download, deletion, and
moving between categories, as well as category addition, deletion, and rename.

```
Usage of ./crane:
  -host string
        IP address to listen on (default "127.0.0.1")
  -port uint
        Port to listen on (default 9090)
  -path string
        Absolute or relative path to papers folder (default "./papers")
  -sci-hub string
        Sci-Hub URL (default "https://sci-hub.se/")
  -user string
        Username for /admin/ endpoints (optional)
  -pass string
        Password for /admin/ endpoints (optional)
```

By default, crane listens on `127.0.0.1:9090` but this is configurable with the
`--host` and `--port` parameters. Authentication is optional but can be enabled
with `--user` and `--pass` parameters; the index is always publicly accessible.

Papers are written to `--path`, stored in directories which serve as paper
categories.
