// SPDX-License-Identifier: AGPL-3.0-only
// Port of Misaka filename_parser.py, pinned upstream. Technical matches use
// explicit ASCII boundaries instead of unsupported lookbehind assertions.
package recognition

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

type Parsed struct {
	Title         string  `json:"title"`
	Season        *int    `json:"season"`
	Episode       *int    `json:"episode"`
	IsMovie       bool    `json:"is_movie"`
	Year          *string `json:"year"`
	Resolution    *string `json:"resolution"`
	VideoCodec    *string `json:"video_codec"`
	AudioCodec    *string `json:"audio_codec"`
	Source        *string `json:"source"`
	Team          *string `json:"team"`
	DynamicRange  *string `json:"dynamic_range"`
	Platform      *string `json:"platform"`
	Effect        *string `json:"effect"`
	OriginalTitle *string `json:"original_title"`
	EnglishName   *string `json:"en_name"`
	RawInput      *string `json:"raw_input"`
}

func String(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

var extensions = map[string]bool{"mkv": true, "mp4": true, "avi": true, "wmv": true, "flv": true, "ts": true, "m2ts": true, "rmvb": true, "rm": true, "mov": true, "webm": true, "mpg": true, "mpeg": true, "vob": true, "iso": true, "bdmv": true, "ogm": true}
var specs = []*regexp.Regexp{
	regexp.MustCompile(`(?i)([0-9]{3,4}[p]|[248]k|[0-9]{3,4}[x][0-9]{3,4})`),
	regexp.MustCompile(`(?i)(H\.?26[45]|[X]26[45]|AVC|HEVC|VC[0-9]?|MPEG[0-9]?|Xvid|DivX|AV1|VP9|10bit|8bit)`),
	regexp.MustCompile(`(?i)(DTS-?HD(?:\.MA|[-\s]MA)?|DTS(?:\.MA|[-\s]MA)?|Atmos|TrueHD|AC-?3|DDP|DD\+|DD|AAC|FLAC|Vorbis|Opus|E-?AC-?3|LPCM|PCM|MP3)(?:(?:\s*|[._-])([0-9]\.[0-9](?:\+[0-9]\.[0-9])?|[0-9]ch))?`),
	regexp.MustCompile(`(?i)(WEB-DL|WEBRIP|WEB-RIP|BDRemux|BDRIP|DVDRIP|HDRip|BLURAY|UHDTV|HDTV|HDDVD|REMUX|UHD|Pdtv|Dvdscr|BLU|WEB|BD|TVRip|DVD)`),
	regexp.MustCompile(`(?i)(HDR10\+|HDR10|HDR|HLG|Dolby\s*Vision|DoVi|DV|SDR|IMAX)`),
	regexp.MustCompile(`(?i)(Baha|Bilibili|Netflix|NF|Amazon|AMZN|DSNP|Crunchyroll|CR|Hulu|HBO|YouTube|YT|playWEB|B-Global|friDay|LINETV|KKTV|ATVP|IQIYI|IQ|CRAMZN|iT|ABEMA|HIDIVE|Funimation|Sentai|VIU|MyVideo|CatchPlay|WeTV|Viki|ADN|Disney\+|AppleTV\+)`),
	regexp.MustCompile(`(?i)(3D|REPACK|HQ|Remastered|Extended|Uncut|Internal|Proper|Pro)`),
}

func asciiWord(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
func specMatches(re *regexp.Regexp, s string) [][]int {
	out := [][]int{}
	for _, m := range re.FindAllStringIndex(s, -1) {
		if (m[0] == 0 || !asciiWord(s[m[0]-1])) && (m[1] == len(s) || !asciiWord(s[m[1]])) {
			out = append(out, m)
		}
	}
	return out
}
func stripSpec(re *regexp.Regexp, s string) string {
	matches := specMatches(re, s)
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		s = s[:m[0]] + " " + s[m[1]:]
	}
	return s
}

var brackets = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)|【[^】]*】|（[^）]*）`)
var leadingGroup = regexp.MustCompile(`^\[([^\]]+)\]`)
var tailGroup = regexp.MustCompile(`[-@]([A-Za-z][A-Za-z0-9]{1,15})$`)
var yearRE = regexp.MustCompile(`(?:19|20)[0-9]{2}`)
var fullEpisode = regexp.MustCompile(`(?i)^(.+?)[\s._-]*S([0-9]{1,2})E([0-9]{1,4})(?:[^a-z0-9_]|$)`)
var splitEpisode = regexp.MustCompile(`(?i)^(.+?)[\s._-]+S([0-9]{1,2})[\s._-]+([0-9]{1,4})(?:[^a-z0-9_]|$)`)
var seasonName = regexp.MustCompile(`(?i)^(.+?)[\s._-]+(?:Season[\s._-]*|S)([0-9]{1,2})(?:\s|$)`)
var namedEpisode = regexp.MustCompile(`(?i)^(.+?)(?:[\s._-]+(?:EP?|Episode)[\s._-]*([0-9]{1,4})|\s*第\s*([零〇一二两三四五六七八九十百千0-9]+)\s*[集话話])(?:[^a-z0-9_]|$)`)
var crossEpisode = regexp.MustCompile(`(?i)^(.+?)[\s._-]+([0-9]{1,2})x([0-9]{1,4})(?:[^a-z0-9_]|$)`)
var trailingEpisode = regexp.MustCompile(`^(.+?)(?:\s*[-_]\s*|\s+)([0-9]{1,4})\s*$`)
var compactHDR = regexp.MustCompile(`(?i)(WEB-DL|HEVC|AVC|H\.?265|H\.?264|x\.?265|x\.?264)(HDR)`)
var groupWords = regexp.MustCompile(`组|組|社|制作|製作|字幕|工作|家族|学园|學園|压制|壓制|发布|發佈|协会|協會|联盟|聯盟|论坛|論壇|中心|屋|团|團|亭|园|園`)
var languageNoise = regexp.MustCompile(`(?i)^[简繁中日英双雙多]+[体文语語]`)
var noise = regexp.MustCompile(`(?i)\b(?:Ma10p|Hi10p|Hi10|Ma10|CHS|CHT|BIG5|GB|JPSC|JP_SC|SUBBED|UNCUT|UNRATE|RERIP|Complete|Version|YYeTs)\b|年龄限制版|年齡限制版|修正版|无修正|未删减|无修正版|無修正版|繁体|繁體|简体|简日|繁日|简中|繁中|简繁|双语|内嵌|內嵌|内封|內封|外挂|外掛|[简繁中日英双雙多]+[体文语語]+`)

func cleanTitle(s string) string {
	s = brackets.ReplaceAllString(s, " ")
	for _, r := range specs {
		s = stripSpec(r, s)
	}
	s = noise.ReplaceAllString(s, " ")
	s = yearRE.ReplaceAllString(s, " ")
	s = strings.NewReplacer(".", " ", "_", " ", "★", " ", "☆", " ").Replace(s)
	return strings.Trim(strings.Join(strings.Fields(s), " "), " -")
}
func cjk(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

var latinWord = regexp.MustCompile(`^[A-Za-z][A-Za-z'\-]*$`)

func multilang(s string) (string, string) {
	if !cjk(s) {
		return s, ""
	}
	w := strings.Fields(s)
	ci, li := -1, -1
	for i, v := range w {
		if ci < 0 && cjk(v) {
			ci = i
		}
		if li < 0 && latinWord.MatchString(v) {
			li = i
		}
	}
	if ci < 0 || li < 0 {
		return s, ""
	}
	if ci < li {
		cut := ci
		for i := ci; i < len(w); i++ {
			if cjk(w[i]) {
				cut = i
			} else if latinWord.MatchString(w[i]) {
				break
			}
		}
		count := 0
		for _, v := range w[cut+1:] {
			if latinWord.MatchString(v) {
				count++
			}
		}
		if count >= 2 {
			return strings.Join(w[:cut+1], " "), strings.Join(w[cut+1:], " ")
		}
	} else {
		count := 0
		for _, v := range w[:ci] {
			if latinWord.MatchString(v) {
				count++
			}
		}
		if count >= 2 {
			return strings.Join(w[ci:], " "), strings.Join(w[:ci], " ")
		}
	}
	return s, ""
}
func finish(p *Parsed, title string) *Parsed {
	title = cleanTitle(title)
	if title == "" {
		return nil
	}
	p.Title = title
	cn, en := multilang(title)
	if en != "" {
		p.Title = cn
		p.EnglishName = String(en)
		p.OriginalTitle = String(title)
	}
	return p
}
func ParseFilename(filename string) *Parsed {
	if len(filename) > MaxText {
		return nil
	}
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(filename), "\\", "/"))
	if strings.TrimSpace(filename) == "" {
		return nil
	}
	ext := filepath.Ext(name)
	if extensions[strings.ToLower(strings.TrimPrefix(ext, "."))] {
		name = strings.TrimSuffix(name, ext)
	}
	name = compactHDR.ReplaceAllString(name, "${1}.${2}")
	p := &Parsed{RawInput: String(filename)}
	fields := []**string{&p.Resolution, &p.VideoCodec, &p.AudioCodec, &p.Source, &p.DynamicRange, &p.Platform, &p.Effect}
	for i, re := range specs {
		m := specMatches(re, name)
		if len(m) > 0 {
			*fields[i] = String(name[m[0][0]:m[0][1]])
		}
	}
	if m := leadingGroup.FindStringSubmatch(name); m != nil {
		p.Team = String(m[1])
	}
	if p.Team == nil && strings.Contains(name, "★") {
		parts := strings.Split(name, "★")
		if len(parts) >= 3 {
			if groupWords.MatchString(parts[0]) {
				p.Team = String(strings.TrimSpace(parts[0]))
				parts = parts[1:]
			}
			titles := []string{}
			var ep *int
			for _, seg := range parts {
				seg = strings.TrimSpace(seg)
				if seg == "" {
					continue
				}
				if n, e := strconv.Atoi(seg); e == nil && len(seg) <= 4 && ep == nil {
					ep = Int(n)
					continue
				}
				skip := extensions[strings.ToLower(seg)] || languageNoise.MatchString(seg)
				for _, re := range specs {
					if len(specMatches(re, seg)) > 0 {
						skip = true
					}
				}
				if !skip {
					titles = append(titles, seg)
				}
			}
			if len(titles) > 0 {
				name = strings.Join(titles, " ")
				if ep != nil {
					name += " " + strconv.Itoa(*ep)
				}
			}
		}
	}
	if p.Team == nil {
		if m := tailGroup.FindStringSubmatch(name); m != nil {
			isSpec := false
			for _, re := range specs {
				if len(specMatches(re, m[1])) > 0 {
					isSpec = true
				}
			}
			if !isSpec {
				p.Team = String(m[1])
			}
		}
	}
	p.Year = String(yearRE.FindString(name))
	for _, re := range []*regexp.Regexp{fullEpisode, splitEpisode, crossEpisode} {
		if m := re.FindStringSubmatch(name); m != nil {
			s, _ := strconv.Atoi(m[2])
			e, _ := strconv.Atoi(m[3])
			p.Season = Int(s)
			p.Episode = Int(e)
			return finish(p, m[1])
		}
	}
	if m := namedEpisode.FindStringSubmatch(name); m != nil {
		number := m[2]
		if number == "" {
			number = m[3]
		}
		n, e := ChineseNumber(number)
		if e == nil {
			base := ParseSearchKeyword(cleanTitle(m[1]))
			p.Season = base.Season
			p.Episode = Int(n)
			return finish(p, base.Title)
		}
	}
	if m := seasonName.FindStringSubmatch(name); m != nil {
		s, _ := strconv.Atoi(m[2])
		p.Season = Int(s)
		return finish(p, m[1])
	}
	temp := name
	if p.Team != nil {
		temp = strings.TrimSuffix(temp, "-"+*p.Team)
		temp = strings.TrimSuffix(temp, "@"+*p.Team)
	}
	temp = cleanTitle(temp)
	if m := trailingEpisode.FindStringSubmatch(temp); m != nil {
		n, _ := strconv.Atoi(m[2])
		if n < 1900 || n > 2099 {
			p.Episode = Int(n)
			base := ParseSearchKeyword(m[1])
			p.Season = base.Season
			return finish(p, base.Title)
		}
	}
	p.IsMovie = true
	return finish(p, temp)
}

var searchFull = regexp.MustCompile(`(?i)^(.+?)\s*S([0-9]{1,2})E([0-9]{1,4})$`)
var searchSeason = []*regexp.Regexp{regexp.MustCompile(`(?i)^(.*?)\s*(?:S|Season)\s*([0-9]{1,2})$`), regexp.MustCompile(`^(.*?)\s*第\s*([零〇一二两三四五六七八九十百千0-9]+)\s*[季部]$`), regexp.MustCompile(`^(.*?)\s*([ⅠⅡⅢⅣⅤⅥⅦⅧⅨⅩⅪⅫ])$`), regexp.MustCompile(`(?i)^(.*?)\s+([IVXLCDM]+)$`), regexp.MustCompile(`^(.*?)\s+([0-9]{1,2})$`)}

func Roman(s string) int {
	wide := map[string]int{"Ⅰ": 1, "Ⅱ": 2, "Ⅲ": 3, "Ⅳ": 4, "Ⅴ": 5, "Ⅵ": 6, "Ⅶ": 7, "Ⅷ": 8, "Ⅸ": 9, "Ⅹ": 10, "Ⅺ": 11, "Ⅻ": 12}
	if n := wide[s]; n != 0 {
		return n
	}
	ascii := map[string]int{"I": 1, "II": 2, "III": 3, "IV": 4, "V": 5, "VI": 6, "VII": 7, "VIII": 8, "IX": 9, "X": 10, "XI": 11, "XII": 12}
	return ascii[strings.ToUpper(s)]
}
func ParseSearchKeyword(keyword string) Parsed {
	keyword = strings.TrimSpace(keyword)
	p := Parsed{Title: keyword, RawInput: String(keyword)}
	if len(keyword) > MaxText {
		return p
	}
	if m := searchFull.FindStringSubmatch(keyword); m != nil {
		s, _ := strconv.Atoi(m[2])
		e, _ := strconv.Atoi(m[3])
		p.Title = strings.TrimSpace(m[1])
		p.Season = Int(s)
		p.Episode = Int(e)
		return p
	}
	for i, re := range searchSeason {
		if m := re.FindStringSubmatch(keyword); m != nil {
			title := strings.TrimSpace(m[1])
			n := 0
			switch i {
			case 2, 3:
				n = Roman(m[2])
			default:
				n, _ = ChineseNumber(m[2])
			}
			if (n > 0 || n == 0 && (i == 0 || i == 1 || i == 4)) && title != "" {
				p.Title = title
				p.Season = Int(n)
				return p
			}
		}
	}
	return p
}
