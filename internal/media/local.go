// SPDX-License-Identifier: AGPL-3.0-only
package media

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type LocalItem struct {
	Item
	FilePath string `json:"filePath"`
	NFOPath  string `json:"nfoPath"`
}
type ScanReport struct {
	Total    int      `json:"total"`
	Success  int      `json:"success"`
	Errors   int      `json:"error"`
	Warnings []string `json:"warnings,omitempty"`
}

var seasonEpisode = regexp.MustCompile(`(?i)(?:\bS(\d{1,3})[ ._-]*E(\d{1,4})\b|\b(\d{1,3})x(\d{1,4})\b)`)

// Match the decimal combined marker supported by the upstream local scanner.
// Keep it separate from episodeOnly so the season is not discarded as title text.
var chineseSeasonEpisode = regexp.MustCompile(`第(\d+)季第(\d+)集`)
var episodeOnly = regexp.MustCompile(`(?i)(?:\bEP?[ ._-]*(\d{1,4})\b|第[ ._-]*(\d{1,4})[ ._-]*[集话話])`)
var yearPattern = regexp.MustCompile(`\b((?:19|20)\d{2})\b`)
var seasonFolder = regexp.MustCompile(`(?i)^(?:season[ ._-]*|s)(\d{1,3})$`)

type nfoData struct {
	Root                                                            xml.Name
	Title, ShowTitle, Year, Season, Episode, TMDBID, TVDBID, IMDBID string
	Unique                                                          []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
}

func (n *nfoData) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	type raw struct {
		Title     string `xml:"title"`
		ShowTitle string `xml:"showtitle"`
		Year      string `xml:"year"`
		Season    string `xml:"season"`
		Episode   string `xml:"episode"`
		TMDBID    string `xml:"tmdbid"`
		TVDBID    string `xml:"tvdbid"`
		IMDBID    string `xml:"imdbid"`
		Unique    []struct {
			Type  string `xml:"type,attr"`
			Value string `xml:",chardata"`
		} `xml:"uniqueid"`
	}
	var r raw
	if e := d.DecodeElement(&r, &start); e != nil {
		return e
	}
	n.Root = start.Name
	n.Title = r.Title
	n.ShowTitle = r.ShowTitle
	n.Year = r.Year
	n.Season = r.Season
	n.Episode = r.Episode
	n.TMDBID = r.TMDBID
	n.TVDBID = r.TVDBID
	n.IMDBID = r.IMDBID
	n.Unique = r.Unique
	for _, u := range n.Unique {
		switch strings.ToLower(u.Type) {
		case "tmdb":
			n.TMDBID = u.Value
		case "tvdb":
			n.TVDBID = u.Value
		case "imdb":
			n.IMDBID = u.Value
		}
	}
	return nil
}
func contained(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && (rel == "." || filepath.IsLocal(rel))
}
func safeLocalPath(root, p string) (string, error) {
	abs, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	resolved, e := filepath.EvalSymlinks(abs)
	if e != nil {
		return "", e
	}
	if !contained(root, resolved) {
		return "", errors.New("local path escapes scan root")
	}
	return resolved, nil
}
func readNFO(root, p string) (*nfoData, error) {
	resolved, e := safeLocalPath(root, p)
	if e != nil {
		return nil, e
	}
	rootHandle, e := os.OpenRoot(root)
	if e != nil {
		return nil, e
	}
	defer rootHandle.Close()
	relative, e := filepath.Rel(root, resolved)
	if e != nil {
		return nil, e
	}
	f, e := rootHandle.Open(relative)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	lr := &io.LimitedReader{R: f, N: 1<<20 + 1}
	d := xml.NewDecoder(lr)
	var n nfoData
	if e = d.Decode(&n); e != nil {
		return nil, e
	}
	if lr.N <= 0 {
		return nil, errors.New("NFO exceeds 1 MiB")
	}
	return &n, nil
}

// ScanLocal walks incrementally and never clears existing scan records or deletes
// source files. The callback decides how to persist a successfully parsed item.
func ScanLocal(ctx context.Context, root string, maxFiles int, emit func(LocalItem) error) (ScanReport, error) {
	report := ScanReport{Warnings: []string{}}
	abs, e := filepath.Abs(root)
	if e != nil {
		return report, e
	}
	abs, e = filepath.EvalSymlinks(abs)
	if e != nil {
		return report, e
	}
	st, e := os.Stat(abs)
	if e != nil {
		return report, e
	}
	if !st.IsDir() {
		return report, errors.New("scan path is not a directory")
	}
	if maxFiles <= 0 {
		maxFiles = MaxScanItems
	}
	e = filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() && d.Name() == ".web-migrations" {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			if len(report.Warnings) < 100 {
				report.Warnings = append(report.Warnings, "skipped symbolic link: "+p)
			}
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(d.Name()), ".xml") {
			return nil
		}
		report.Total++
		if report.Total > maxFiles {
			return errors.New("local scan exceeds file limit")
		}
		item, err := ParseLocal(abs, p)
		if err != nil {
			report.Errors++
			if len(report.Warnings) < 100 {
				report.Warnings = append(report.Warnings, filepath.Base(p)+": "+err.Error())
			}
			return nil
		}
		if err = emit(item); err != nil {
			return err
		}
		report.Success++
		return nil
	})
	return report, e
}
func ParseLocal(root, p string) (LocalItem, error) {
	var out LocalItem
	resolved, e := safeLocalPath(root, p)
	if e != nil {
		return out, e
	}
	rootHandle, e := os.OpenRoot(root)
	if e != nil {
		return out, e
	}
	defer rootHandle.Close()
	relative, e := filepath.Rel(root, resolved)
	if e != nil {
		return out, e
	}
	f, e := rootHandle.Open(relative)
	if e != nil {
		return out, e
	}
	d := xml.NewDecoder(io.LimitReader(f, 4096))
	valid := false
	for {
		tok, err := d.Token()
		if err != nil {
			f.Close()
			return out, fmt.Errorf("invalid danmaku XML: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok {
			valid = start.Name.Local == "i"
			break
		}
	}
	f.Close()
	if !valid {
		return out, errors.New("XML root is not danmaku i")
	}
	base := strings.TrimSuffix(filepath.Base(resolved), filepath.Ext(resolved))
	dir := filepath.Dir(resolved)
	title := base
	var season, episode *int
	idx := -1
	if match := seasonEpisode.FindStringSubmatchIndex(base); match != nil {
		idx = match[0]
		groups := seasonEpisode.FindStringSubmatch(base)
		sv, ev := groups[1], groups[2]
		if sv == "" {
			sv, ev = groups[3], groups[4]
		}
		season = intPointer(sv)
		episode = intPointer(ev)
	} else if match := chineseSeasonEpisode.FindStringSubmatchIndex(base); match != nil {
		sv := intPointer(base[match[2]:match[3]])
		ev := intPointer(base[match[4]:match[5]])
		// An overflowing field leaves the entire marker literal, without falling
		// back to its episode fragment. Folder and NFO metadata still apply below.
		if sv != nil && ev != nil {
			idx = match[0]
			season, episode = sv, ev
		}
	} else if match := episodeOnly.FindStringSubmatchIndex(base); match != nil {
		idx = match[0]
		g := episodeOnly.FindStringSubmatch(base)
		v := g[1]
		if v == "" {
			v = g[2]
		}
		episode = intPointer(v)
		one := 1
		season = &one
	}
	if idx >= 0 {
		title = strings.Trim(base[:idx], " ._-[()]")
	}
	if title == "" {
		title = filepath.Base(dir)
	}
	if g := seasonFolder.FindStringSubmatch(filepath.Base(dir)); g != nil {
		season = intPointer(g[1])
		if title == filepath.Base(dir) || idx == 0 {
			title = filepath.Base(filepath.Dir(dir))
		}
	}
	out = LocalItem{Item: Item{Title: title, MediaType: "movie", Season: season, Episode: episode}, FilePath: resolved}
	if season != nil || episode != nil {
		out.MediaType = "tv_series"
	}
	if y := yearPattern.FindStringSubmatch(base); y != nil {
		out.Year = intPointer(y[1])
		out.Title = strings.Trim(strings.Replace(out.Title, y[1], "", 1), " ._-()[]")
	}
	candidates := []string{filepath.Join(dir, base+".nfo"), filepath.Join(dir, "movie.nfo"), filepath.Join(dir, "tvshow.nfo")}
	var own *nfoData
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			own, e = readNFO(root, candidate)
			if e != nil {
				return out, fmt.Errorf("NFO: %w", e)
			}
			out.NFOPath = candidate
			break
		}
	}
	var parent *nfoData
	for ancestor, depth := dir, 0; depth < 4 && contained(root, ancestor); ancestor, depth = filepath.Dir(ancestor), depth+1 {
		candidate := filepath.Join(ancestor, "tvshow.nfo")
		if _, err := os.Stat(candidate); err == nil {
			parent, e = readNFO(root, candidate)
			if e != nil {
				return out, e
			}
			break
		}
		if ancestor == root {
			break
		}
	}
	if own != nil {
		if own.Root.Local == "movie" {
			out.MediaType = "movie"
		} else if own.Root.Local == "tvshow" || own.Root.Local == "episodedetails" || own.Root.Local == "season" {
			out.MediaType = "tv_series"
		}
		if own.ShowTitle != "" {
			out.Title = own.ShowTitle
		} else if own.Title != "" && own.Root.Local != "episodedetails" {
			out.Title = own.Title
		}
		if own.Season != "" {
			out.Season = intPointer(own.Season)
		}
		if own.Episode != "" {
			out.Episode = intPointer(own.Episode)
		}
		if own.Year != "" {
			out.Year = intPointer(own.Year)
		}
		out.TMDBID = own.TMDBID
		out.TVDBID = own.TVDBID
		out.IMDBID = own.IMDBID
	}
	if parent != nil && out.MediaType == "tv_series" {
		if parent.Title != "" {
			out.Title = parent.Title
		}
		if parent.Year != "" {
			out.Year = intPointer(parent.Year)
		}
		if parent.TMDBID != "" {
			out.TMDBID = parent.TMDBID
		}
		if parent.TVDBID != "" {
			out.TVDBID = parent.TVDBID
		}
		if parent.IMDBID != "" {
			out.IMDBID = parent.IMDBID
		}
		if out.NFOPath == "" {
			for ancestor, depth := dir, 0; depth < 4 && contained(root, ancestor); ancestor, depth = filepath.Dir(ancestor), depth+1 {
				candidate := filepath.Join(ancestor, "tvshow.nfo")
				if _, err := os.Stat(candidate); err == nil {
					out.NFOPath = candidate
					break
				}
				if ancestor == root {
					break
				}
			}
		}
	}
	if strings.TrimSpace(out.Title) == "" {
		return out, errors.New("cannot infer media title")
	}
	for ancestor, depth := dir, 0; depth < 3 && contained(root, ancestor); ancestor, depth = filepath.Dir(ancestor), depth+1 {
		names := []string{base + "-poster.jpg", "poster.jpg", "poster.png", "cover.jpg"}
		if out.Season != nil {
			names = append([]string{fmt.Sprintf("season%02d-poster.jpg", *out.Season)}, names...)
		}
		for _, name := range names {
			candidate := filepath.Join(ancestor, name)
			if st, err := os.Stat(candidate); err == nil && st.Mode().IsRegular() {
				safe, err := safeLocalPath(root, candidate)
				if err != nil {
					return out, err
				}
				out.PosterURL = safe
				return out, nil
			}
		}
		if ancestor == root {
			break
		}
	}
	return out, nil
}
func ParseInteger(v string) *int {
	n, e := strconv.Atoi(v)
	if e != nil {
		return nil
	}
	return &n
}
