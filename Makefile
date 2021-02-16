.POSIX:
.SUFFIXES:

GO = go
RM = rm
GOFLAGS =
PREFIX = /usr/local
BINDIR = $(PREFIX)/bin
SHAREDIR = $(PREFIX)/share/crane

goflags = $(GOFLAGS)

all: crane

crane:
	$(GO) build $(goflags) -ldflags "-X main.buildPrefix=$(PREFIX)"

clean:
	$(RM) -f crane

install: all
	mkdir -p $(DESTDIR)$(BINDIR)
	mkdir -p $(DESTDIR)$(SHAREDIR)
	cp -f crane $(DESTDIR)$(BINDIR)
	cp -R templates $(DESTDIR)$(SHAREDIR)

