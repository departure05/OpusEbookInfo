package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"github.com/mholt/archives"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	AppName    = "GOEbook Worker Helper"
	AppVersion = "1.0.0"
)

var (
	socketPath string
	awakeSecs  int // 0 means "wait indefinitely"

	lastActivity   time.Time
	lastActivityMu sync.RWMutex

	shutdownCh = make(chan struct{})
)

func touchActivity() {
	lastActivityMu.Lock()
	lastActivity = time.Now()
	lastActivityMu.Unlock()
}


const dcNS = "http://purl.org/dc/elements/1.1/"

type xmlContainer struct {
	Rootfiles struct {
		Rootfile []struct {
			FullPath string `xml:"full-path,attr"`
		} `xml:"rootfile"`
	} `xml:"rootfiles"`
}

type metaElement struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Value   string     `xml:",chardata"`
}

type manifestItem struct {
	ID         string `xml:"id,attr"`
	Href       string `xml:"href,attr"`
	MediaType  string `xml:"media-type,attr"`
	Properties string `xml:"properties,attr"`
}

type opfPackage struct {
	Metadata struct {
		Items []metaElement `xml:",any"`
	} `xml:"metadata"`
	Manifest struct {
		Items []manifestItem `xml:"item"`
	} `xml:"manifest"`
	Guide *struct {
		References []struct {
			Type string `xml:"type,attr"`
			Href string `xml:"href,attr"`
		} `xml:"reference"`
	} `xml:"guide"`
	Spine struct {
		Toc string `xml:"toc,attr"`
	} `xml:"spine"`
}

func findManifestItem(pkg *opfPackage, id string) *manifestItem {
	for i := range pkg.Manifest.Items {
		if pkg.Manifest.Items[i].ID == id {
			return &pkg.Manifest.Items[i]
		}
	}
	return nil
}

func resolveZipPath(opfDir, href string) string {
	decoded, err := url.QueryUnescape(href)
	if err != nil {
		decoded = href
	}
	return path.Clean(path.Join(opfDir, decoded))
}

func getAttr(attrs []xml.Attr, local string) string {
	for _, a := range attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}

func addValue(m map[string]interface{}, key string, val interface{}) {
	existing, ok := m[key]
	if !ok {
		m[key] = map[string]interface{}{"value": val}
		return
	}
	entry, ok := existing.(map[string]interface{})
	if !ok {
		m[key] = map[string]interface{}{"value": val}
		return
	}
	if slice, ok := entry["value"].([]interface{}); ok {
		entry["value"] = append(slice, val)
		return
	}
	entry["value"] = []interface{}{entry["value"], val}
}

func decodeValue(s string) interface{} {
	t := strings.TrimSpace(s)
	if len(t) > 0 && (t[0] == '{' || t[0] == '[') {
		var v interface{}
		if err := json.Unmarshal([]byte(t), &v); err == nil {
			return v
		}
	}
	return s
}

var canonicalFields = map[string]string{
	// OPF dc:* local names
	"title":       "meta:title",
	"description": "meta:summary", // also PDF Info-dict "Subject"
	"subject":     "meta:genres",
	"publisher":   "meta:publisher",
	"language":    "meta:language",
	"format":      "meta:format",
	"type":        "meta:type",
	"rights":      "meta:copyright",
	"date":        "meta:releasedate",
	"creator":     "meta:authors",      // base (roleless) dc:creator
	"contributor": "meta:contributors", // base (roleless) dc:contributor

	// dc:date opf:event values / dcterms event aliases
	"creation":     "meta:date:creation",
	"modification": "meta:date:modification",
	"publication":  "meta:date:publication",

	// calibre <meta> columns (OPF)
	"calibre:series":       "meta:series",
	"calibre:series_index": "meta:volume",

	// PDF Info-dict field names
	"author":       "meta:authors",
	"keywords":     "meta:tags",
	"creationdate": "meta:date:creation",
	"moddate":      "meta:date:modification",
	// PDF XMP property names (spelled differently from the Info dict)
	"createdate": "meta:date:creation",
	"modifydate": "meta:date:modification",

	// ComicInfo.xml tag names (lower-cased)
	"summary":     "meta:summary",
	"genre":       "meta:genres",
	"languageiso": "meta:language",
	"writer":      "meta:authors",
	"tags":        "meta:tags",
	"pagecount":   "meta:pages", // shared with the PDF page-tree count below
	"number":      "meta:chapters",
	"volume":      "meta:volume",
	"translator":  "meta:translator",
	"editor":      "meta:editor",
	"inker":       "meta:inker",
	"penciller":   "meta:penciller",
	"letterer":    "meta:letterer",
	"colorist":    "meta:colorist",
	"coverartist": "meta:cover_artist",

	// ISBN, however each format spells it: an OPF opf:scheme/identifier-type
	"isbn":   "meta:isbn",
	"isbn10": "meta:isbn",
	"isbn13": "meta:isbn",
	"gtin":   "meta:isbn", // ComicInfo
}

func canonicalOrDefault(rawName, def string) string {
	if key, ok := canonicalFields[strings.ToLower(rawName)]; ok {
		return key
	}
	return def
}

func canonicalExtra(key string) map[string]interface{} {
	switch {
	case strings.HasPrefix(key, "meta:date:"), key == "meta:releasedate":
		return map[string]interface{}{"type": "datetime"}
	case key == "meta:pages":
		return map[string]interface{}{"type": "number"}
	}
	return nil
}

var roleNameToCanonical = map[string]string{
	"author":      "meta:authors",
	"creator":     "meta:authors",
	"contributor": "meta:contributors",
	"other":       "meta:contributors",
}

func roleCanonicalKey(roleName string) string {
	if key, ok := roleNameToCanonical[roleName]; ok {
		return key
	}
	return "meta:" + roleName
}

var (
	htmlParagraphOpenRe  = regexp.MustCompile(`(?i)<p[^>]*>`)
	htmlParagraphCloseRe = regexp.MustCompile(`(?i)</p\s*>`)
)

func stripHTMLParagraphs(s string) string {
	s = htmlParagraphCloseRe.ReplaceAllString(s, "\n")
	s = htmlParagraphOpenRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func normalizeCanonicalValue(key, val string) string {
	if key == "meta:summary" {
		return stripHTMLParagraphs(val)
	}
	return val
}

func setValue(info map[string]interface{}, key string, val interface{}, extra map[string]interface{}) {
	if _, exists := info[key]; exists {
		return
	}
	entry := map[string]interface{}{"value": val}
	for k, v := range extra {
		entry[k] = v
	}
	info[key] = entry
}

func setCanonicalStr(info map[string]interface{}, rawName, val string) bool {
	key, ok := canonicalFields[strings.ToLower(rawName)]
	if !ok {
		return false
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return true // recognized concept, just nothing to store
	}
	setValue(info, key, normalizeCanonicalValue(key, val), canonicalExtra(key))
	return true
}

var onixIdentifierTypeCodes = map[string]string{
	"01": "proprietary",
	"02": "isbn10",
	"03": "gtin13",
	"04": "upc",
	"06": "doi",
	"15": "isbn13",
	"22": "urn",
	"34": "issn13",
}

var dctermsElementAlias = map[string]bool{
	"title": true, "creator": true, "subject": true, "description": true,
	"publisher": true, "contributor": true, "type": true, "format": true,
	"source": true, "language": true, "relation": true, "coverage": true,
	"rights": true,
}

var dctermsDateEventAlias = map[string]string{
	"modified": "modification",
	"created":  "creation",
	"issued":   "publication",
}

var marcRelatorCodes = map[string]string{
	"aut": "author", "aqt": "author_in_quotations", "aft": "author_of_afterword",
	"aui": "author_of_introduction", "ant": "bibliographic_antecedent", "arr": "arranger",
	"art": "artist", "asn": "associated_name", "aud": "author_of_dialog",
	"bkp": "book_producer", "clb": "collaborator", "cmm": "commentator",
	"com": "compiler", "cre": "creator", "ctb": "contributor", "cur": "curator",
	"edt": "editor", "egr": "engraver", "fac": "facsimilist", "ill": "illustrator",
	"ins": "inscriber", "lyr": "lyricist", "mus": "musician", "nrt": "narrator",
	"org": "originator", "oth": "other", "pbl": "publisher", "pht": "photographer",
	"red": "redactor", "rev": "reviewer", "spn": "sponsor", "trc": "transcriber",
	"trl": "translator", "voc": "vocalist", "wac": "writer_of_added_commentary",
	"wal": "writer_of_added_lyrics", "wat": "writer_of_added_text",
}

func expandRole(raw string) string {
	code := strings.ToLower(strings.TrimSpace(raw))
	if code == "" {
		return ""
	}
	if name, ok := marcRelatorCodes[code]; ok {
		return name
	}
	return strings.Join(strings.Fields(code), "_")
}

type dcOccurrence struct {
	key          string // final info key, e.g. "meta:title", "meta:isbn", "dc:identifier:doi" (identifier types outside the harmonization table keep their raw dc:identifier:<type> key)
	id           string
	value        string
	fileAsAttr   string // opf:file-as / file-as attribute directly on the element (EPUB2 style)
	roleAttr     string // opf:role / role attribute directly on the element
	isIdentifier bool
	attrScheme   string // opf:scheme attribute, only meaningful when isIdentifier
	mergeable    bool   // true: same-key entries join with "; ". false: kept as separate array entries (dc:date)
	docIndex     int
}

var calibreDatatypeMap = map[string]string{
	"text":        "string",
	"comments":    "string",
	"int":         "number",
	"float":       "double",
	"bool":        "bool",
	"date":        "datetime",
	"rating":      "stars",
	"enumeration": "list",
	"series":      "string",
	"composite":   "string",
}

func parseCalibreCustomColumn(info map[string]interface{}, name, content string) {
	var raw interface{}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return
	}
	extract := func(m map[string]interface{}) map[string]interface{} {
		entry := map[string]interface{}{}
		if v, ok := m["#value#"]; ok {
			switch val := v.(type) {
			case string:
				entry["value"] = val
			case bool:
				if val {
					entry["value"] = "1"
				} else {
					entry["value"] = "0"
				}
			case []interface{}:
				parts := make([]string, len(val))
				for i, item := range val {
					parts[i] = fmt.Sprint(item)
				}
				entry["value"] = strings.Join(parts, "; ")
			default:
				if b, err := json.Marshal(val); err == nil {
					entry["value"] = string(b)
				}
			}
		}
		if v, ok := m["datatype"]; ok {
			if s, ok := v.(string); ok {
				if mapped, found := calibreDatatypeMap[s]; found {
					entry["type"] = mapped
				} else {
					entry["type"] = s
				}
			} else {
				entry["type"] = v
			}
		}
		if v, ok := m["label"]; ok {
			entry["name"] = v
		}
		if v, ok := m["name"]; ok {
			entry["label"] = v
		}
		if len(entry) == 0 {
			return nil
		}
		return entry
	}

	if strings.HasPrefix(name, "calibre:user_metadata:") {
		colID := strings.TrimPrefix(name, "calibre:user_metadata:")
		if m, ok := raw.(map[string]interface{}); ok {
			if entry := extract(m); entry != nil {
				info["meta:calibre:custom:"+colID] = entry
			}
		}
		return
	}

	// Aggregate form: single meta tag mapping every custom column at once.
	if m, ok := raw.(map[string]interface{}); ok {
		for colID, v := range m {
			if colMeta, ok := v.(map[string]interface{}); ok {
				if entry := extract(colMeta); entry != nil {
					info["meta:calibre:custom:"+colID] = entry
				}
			}
		}
	}
}

func buildInfo(pkg *opfPackage) (map[string]interface{}, string) {
	info := map[string]interface{}{}
	coverID := ""

	type indexedMeta struct {
		item metaElement
		idx  int
	}

	var occurrences []dcOccurrence
	var metas []indexedMeta

	for i, item := range pkg.Metadata.Items {
		if item.XMLName.Space == dcNS {
			local := item.XMLName.Local
			val := strings.TrimSpace(item.Value)
			if val == "" {
				continue // no value, nothing to keep
			}
			key := canonicalOrDefault(local, "dc:"+local)
			mergeable := true
			isIdentifier := local == "identifier"
			attrScheme := ""

			switch local {
			case "identifier":
				attrScheme = getAttr(item.Attrs, "scheme")
			case "date":
				mergeable = false
				if event := getAttr(item.Attrs, "event"); event != "" {
					key = canonicalOrDefault(event, "dc:date:"+event)
				}
			}

			occurrences = append(occurrences, dcOccurrence{
				key:          key,
				id:           getAttr(item.Attrs, "id"),
				value:        val,
				fileAsAttr:   getAttr(item.Attrs, "file-as"),
				roleAttr:     getAttr(item.Attrs, "role"),
				isIdentifier: isIdentifier,
				attrScheme:   attrScheme,
				mergeable:    mergeable,
				docIndex:     i,
			})
			continue
		}

		if item.XMLName.Local == "meta" {
			if getAttr(item.Attrs, "name") == "cover" {
				coverID = getAttr(item.Attrs, "content")
				continue
			}
			metas = append(metas, indexedMeta{item: item, idx: i})
		}
	}

	fileAsByID := map[string]string{}
	displaySeqByID := map[string]float64{}
	identifierTypeByID := map[string]string{}
	roleByID := map[string][]string{}

	// Detect schema: containers before processing meta refinements.
	type schemaContainer struct{ localName string }
	schemaContainers := map[string]schemaContainer{}
	for _, im := range metas {
		item := im.item
		if getAttr(item.Attrs, "name") != "" {
			continue
		}
		prop := getAttr(item.Attrs, "property")
		refines := getAttr(item.Attrs, "refines")
		if refines != "" || !strings.HasPrefix(prop, "schema:") {
			continue
		}
		val := strings.TrimSpace(item.Value)
		if !strings.HasPrefix(val, "schema:") {
			continue
		}
		if id := getAttr(item.Attrs, "id"); id != "" {
			schemaContainers[id] = schemaContainer{localName: strings.TrimPrefix(prop, "schema:")}
		}
	}

	var schemaFieldOrder []string
	schemaFieldValues := map[string][]string{}

	for _, im := range metas {
		item := im.item

		if name := getAttr(item.Attrs, "name"); name != "" {
			content := getAttr(item.Attrs, "content")
			if content == "" {
				continue
			}
			if name == "calibre:user_metadata" || strings.HasPrefix(name, "calibre:user_metadata:") {
				parseCalibreCustomColumn(info, name, content)
				continue
			}
			if canonical, ok := canonicalFields[strings.ToLower(name)]; ok {
				setValue(info, canonical, content, nil)
				continue
			}
			addValue(info, "meta:"+name, decodeValue(content))
			continue
		}

		prop := getAttr(item.Attrs, "property")
		if prop == "" {
			continue
		}
		val := strings.TrimSpace(item.Value)
		refines := strings.TrimPrefix(getAttr(item.Attrs, "refines"), "#")

		if refines != "" {
			if container, ok := schemaContainers[refines]; ok && strings.HasPrefix(prop, "schema:") {
				if val == "" {
					continue
				}
				subfield := strings.TrimPrefix(prop, "schema:")
				key := "meta:schema:" + container.localName + ":" + subfield
				if _, seen := schemaFieldValues[key]; !seen {
					schemaFieldOrder = append(schemaFieldOrder, key)
				}
				schemaFieldValues[key] = append(schemaFieldValues[key], val)
				continue
			}
			switch prop {
			case "file-as":
				fileAsByID[refines] = val
			case "display-seq":
				if f, err := strconv.ParseFloat(val, 64); err == nil {
					displaySeqByID[refines] = f
				}
			case "identifier-type":
				scheme := strings.ToLower(getAttr(item.Attrs, "scheme"))
				resolved := strings.ToLower(val)
				if strings.Contains(scheme, "onix") {
					if name, ok := onixIdentifierTypeCodes[resolved]; ok {
						resolved = name
					}
				}
				identifierTypeByID[refines] = resolved
			case "role":
				roleByID[refines] = append(roleByID[refines], val)
			}
			// Any other refinement (title-type, group-position, collection
			// metadata, etc.) doesn't modify a value we track, so it's dropped.
			continue
		}

		if val == "" {
			continue
		}

		// Skip emitting the schema container element itself as a value.
		if c, ok := schemaContainers[getAttr(item.Attrs, "id")]; ok && strings.HasPrefix(prop, "schema:") && strings.TrimPrefix(prop, "schema:") == c.localName {
			continue
		}

		suffix := strings.TrimPrefix(prop, "dcterms:")
		if suffix != prop { // property was "dcterms:X"
			if event, ok := dctermsDateEventAlias[suffix]; ok {
				occurrences = append(occurrences, dcOccurrence{
					key: canonicalOrDefault(event, "dc:date:"+event), value: val, docIndex: im.idx,
				})
				continue
			}
			if suffix == "date" {
				occurrences = append(occurrences, dcOccurrence{
					key: canonicalOrDefault("date", "dc:date"), value: val, docIndex: im.idx,
				})
				continue
			}
			if dctermsElementAlias[suffix] {
				occurrences = append(occurrences, dcOccurrence{
					key: canonicalOrDefault(suffix, "dc:"+suffix), value: val, mergeable: true, docIndex: im.idx,
				})
				continue
			}
		}

		addValue(info, "meta:"+prop, decodeValue(val))
	}

	// Emit accumulated schema: container fields.
	for _, key := range schemaFieldOrder {
		info[key] = map[string]interface{}{"value": strings.Join(schemaFieldValues[key], "; ")}
	}

	for i := range occurrences {
		if !occurrences[i].isIdentifier {
			continue
		}
		suffix := ""
		if resolved, ok := identifierTypeByID[occurrences[i].id]; ok && resolved != "" {
			suffix = resolved
		} else if occurrences[i].id != "" {
			suffix = occurrences[i].id
		} else if occurrences[i].attrScheme != "" {
			suffix = occurrences[i].attrScheme
		}
		if suffix != "" {
			occurrences[i].key = canonicalOrDefault(suffix, "dc:identifier:"+suffix)
		}
	}

	// Generate stacked role fields so they go through the same
	// grouping/ordering/merge mechanism as everything else.
	var roleOccurrences []dcOccurrence
	for _, occ := range occurrences {
		var roles []string
		if occ.roleAttr != "" {
			roles = append(roles, strings.Fields(occ.roleAttr)...)
		}
		roles = append(roles, roleByID[occ.id]...)
		for _, r := range roles {
			roleName := expandRole(r)
			if roleName == "" {
				continue
			}
			clone := occ
			clone.key = roleCanonicalKey(roleName)
			roleOccurrences = append(roleOccurrences, clone)
		}
	}
	occurrences = append(occurrences, roleOccurrences...)

	type group struct {
		entries   []dcOccurrence
		mergeable bool
	}
	groups := map[string]*group{}
	var groupOrder []string

	for _, occ := range occurrences {
		g, ok := groups[occ.key]
		if !ok {
			g = &group{mergeable: occ.mergeable}
			groups[occ.key] = g
			groupOrder = append(groupOrder, occ.key)
		}
		g.entries = append(g.entries, occ)
	}

	for _, key := range groupOrder {
		g := groups[key]
		entries := g.entries

		sort.SliceStable(entries, func(i, j int) bool {
			si, hasI := displaySeqByID[entries[i].id]
			sj, hasJ := displaySeqByID[entries[j].id]
			if hasI && hasJ {
				return si < sj
			}
			if hasI != hasJ {
				return hasI
			}
			return entries[i].docIndex < entries[j].docIndex
		})

		seen := map[string]bool{}
		values := make([]string, 0, len(entries))
		fileAsValues := make([]string, 0, len(entries))
		for _, e := range entries {
			dedupKey := strings.ToLower(strings.TrimSpace(e.value))
			if seen[dedupKey] {
				continue
			}
			seen[dedupKey] = true
			values = append(values, e.value)

			fa := e.fileAsAttr
			if fa == "" {
				fa = fileAsByID[e.id]
			}
			if fa != "" {
				fileAsValues = append(fileAsValues, fa)
			}
		}

		entry := map[string]interface{}{}
		switch {
		case len(values) == 1:
			entry["value"] = values[0]
		case g.mergeable:
			entry["value"] = strings.Join(values, "; ")
		default:
			arr := make([]interface{}, len(values))
			for i, v := range values {
				arr[i] = v
			}
			entry["value"] = arr
		}

		if len(fileAsValues) > 0 {
			entry["ordered"] = strings.Join(fileAsValues, "; ")
		}
		for k, v := range canonicalExtra(key) {
			entry[k] = v
		}
		info[key] = entry
	}

	return info, coverID
}

func resolveCoverHref(pkg *opfPackage, coverID string) string {
	if coverID != "" {
		for _, m := range pkg.Manifest.Items {
			if m.ID == coverID {
				return m.Href
			}
		}
	}
	for _, m := range pkg.Manifest.Items {
		for _, p := range strings.Fields(m.Properties) {
			if p == "cover-image" {
				return m.Href
			}
		}
	}
	if pkg.Guide != nil {
		for _, r := range pkg.Guide.References {
			if strings.EqualFold(r.Type, "cover") {
				return r.Href
			}
		}
	}
	return ""
}

func findZipFile(zr *zip.ReadCloser, name string) *zip.File {
	trimmed := strings.TrimPrefix(name, "/")
	for _, f := range zr.File {
		if f.Name == name || strings.TrimPrefix(f.Name, "/") == trimmed {
			return f
		}
	}
	return nil
}

func readZipFile(zr *zip.ReadCloser, name string) ([]byte, error) {
	f := findZipFile(zr, name)
	if f == nil {
		return nil, fmt.Errorf("file not found in archive: %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type ncxNavPoint struct {
	Children []ncxNavPoint `xml:"navPoint"`
}

type ncxDoc struct {
	NavMap struct {
		NavPoints []ncxNavPoint `xml:"navPoint"`
	} `xml:"navMap"`
}

func countNavPoints(points []ncxNavPoint) int {
	total := len(points)
	for _, p := range points {
		total += countNavPoints(p.Children)
	}
	return total
}

func countNavXHTMLTocEntries(data []byte) (int, bool) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	inToc := false
	depth := 0
	count := 0
	found := false

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "nav" {
				if inToc {
					depth++
					continue
				}
				for _, a := range t.Attr {
					if a.Name.Local == "type" && strings.Contains(a.Value, "toc") {
						inToc = true
						found = true
						depth = 1
						break
					}
				}
				continue
			}
			if inToc && t.Name.Local == "li" {
				count++
			}
		case xml.EndElement:
			if inToc && t.Name.Local == "nav" {
				depth--
				if depth == 0 {
					return count, true
				}
			}
		}
	}
	return count, found
}

func countTocEntries(readRel func(href string) ([]byte, error), pkg *opfPackage) (int, bool) {
	for _, m := range pkg.Manifest.Items {
		for _, p := range strings.Fields(m.Properties) {
			if p != "nav" {
				continue
			}
			data, err := readRel(m.Href)
			if err != nil {
				return 0, false
			}
			return countNavXHTMLTocEntries(data)
		}
	}

	if pkg.Spine.Toc != "" {
		if item := findManifestItem(pkg, pkg.Spine.Toc); item != nil {
			data, err := readRel(item.Href)
			if err == nil {
				var doc ncxDoc
				if xml.Unmarshal(data, &doc) == nil {
					return countNavPoints(doc.NavMap.NavPoints), true
				}
			}
		}
	}

	return 0, false
}

type coverSource struct {
	materialize func(dest string) error
	close       func() error
}

func newCoverSource(materialize func(dest string) error, closeFn func() error) *coverSource {
	return &coverSource{
		materialize: func(dest string) error {
			if err := materialize(dest); err != nil {
				if fallback, ok := defaultCoverPath(); ok {
					return linkOrCopyFile(fallback, dest)
				}
				return err
			}
			return nil
		},
		close: closeFn,
	}
}

func noCoverSource(closeFn func() error) *coverSource {
	return newCoverSource(func(string) error { return fmt.Errorf("no cover found") }, closeFn)
}

func defaultCoverPath() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	candidate := filepath.Join(filepath.Dir(exe), "no_cover.jpg")
	if _, err := os.Stat(candidate); err != nil {
		return "", false
	}
	return candidate, true
}

type coverPick struct {
	internalName string
	loosePath    string
}

func (p coverPick) empty() bool { return p.internalName == "" && p.loosePath == "" }

func resolveCover(specified coverPick, findInternal func(name string) (string, bool), sourceDir string, lastResort func() string) coverPick {
	if !specified.empty() {
		return specified
	}
	if findInternal != nil {
		if real, ok := findInternal("cover.jpg"); ok {
			return coverPick{internalName: real}
		}
	}
	if sourceDir != "" {
		if loose, ok := findFileInDir(sourceDir, "cover.jpg"); ok {
			return coverPick{loosePath: loose}
		}
	}
	if lastResort != nil {
		if name := lastResort(); name != "" {
			return coverPick{internalName: name}
		}
	}
	return coverPick{}
}

func findFileInDir(dir, target string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(e.Name(), target) {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

func coverSourceFrom(pick coverPick, openInternal func(name, dest string) error, closeFn func() error) *coverSource {
	switch {
	case pick.loosePath != "":
		loosePath := pick.loosePath
		return newCoverSource(func(dest string) error { return linkOrCopyFile(loosePath, dest) }, closeFn)
	case pick.internalName != "" && openInternal != nil:
		name := pick.internalName
		return newCoverSource(func(dest string) error { return openInternal(name, dest) }, closeFn)
	default:
		return noCoverSource(closeFn)
	}
}

func (c *coverSource) materializeToTemp() string {
	dest, err := reserveTempCoverPath()
	if err != nil {
		return ""
	}
	if err := c.materialize(dest); err != nil {
		os.Remove(dest)
		return ""
	}
	return dest
}

func reserveTempCoverPath() (string, error) {
	f, err := os.CreateTemp("", "epubcover_*.jpg")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return name, nil
}

func linkOrCopyFile(src, dest string) error {
	os.Remove(dest)
	if err := os.Symlink(src, dest); err == nil {
		return nil
	}
	return copyFile(src, dest)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func loadEpub(epubPath string) (map[string]interface{}, *coverSource, error) {
	if strings.TrimSpace(epubPath) == "" {
		return nil, nil, fmt.Errorf("path is empty")
	}

	dir := filepath.Dir(epubPath)

	if opfData, err := os.ReadFile(epubPath + ".opf"); err == nil {
		return finalizeLoad(loadFromExternalOpf(opfData, dir))
	}
	if opfData, err := os.ReadFile(filepath.Join(dir, "metadata.opf")); err == nil {
		return finalizeLoad(loadFromExternalOpf(opfData, dir))
	}
	if strings.EqualFold(filepath.Ext(epubPath), ".pdf") {
		return finalizeLoad(loadPdf(epubPath))
	}
	switch strings.ToLower(filepath.Ext(epubPath)) {
	case ".cbz", ".cbr", ".cb7", ".cbt":
		return finalizeLoad(loadComicArchive(epubPath))
	}
	if strings.EqualFold(filepath.Ext(epubPath), ".epub") {
		return finalizeLoad(loadFromZip(epubPath))
	}
	return nil, nil, fmt.Errorf("%v is not a valid file for this util", epubPath)
}

func finalizeLoad(info map[string]interface{}, cover *coverSource, err error) (map[string]interface{}, *coverSource, error) {
	if err != nil {
		return nil, nil, err
	}
	stripObjectValues(info)
	return info, cover, nil
}

func finalizeOpfInfo(pkg *opfPackage, readRel func(href string) ([]byte, error)) (map[string]interface{}, string) {
	info, coverID := buildInfo(pkg)
	coverHref := resolveCoverHref(pkg, coverID)
	if n, ok := countTocEntries(readRel, pkg); ok {
		info["meta:toc"] = map[string]interface{}{"value": n}
	}
	return info, coverHref
}

func loadFromExternalOpf(opfData []byte, dir string) (map[string]interface{}, *coverSource, error) {
	var pkg opfPackage
	if err := xml.Unmarshal(opfData, &pkg); err != nil {
		return nil, nil, fmt.Errorf("cannot parse metadata.opf: %v", err)
	}

	resolvePath := func(href string) string {
		decoded, err := url.QueryUnescape(href)
		if err != nil {
			decoded = href
		}
		return filepath.Join(dir, filepath.FromSlash(decoded))
	}
	readRel := func(href string) ([]byte, error) {
		return os.ReadFile(resolvePath(href))
	}

	info, coverHref := finalizeOpfInfo(&pkg, readRel)

	var specified coverPick
	if coverHref != "" {
		specified = coverPick{loosePath: resolvePath(coverHref)}
	}
	pick := resolveCover(specified, nil, dir, nil)
	cover := coverSourceFrom(pick, nil, func() error { return nil })

	return info, cover, nil
}

func loadFromZip(epubPath string) (map[string]interface{}, *coverSource, error) {
	zr, err := zip.OpenReader(epubPath)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open epub: %v", err)
	}

	containerData, err := readZipFile(zr, "META-INF/container.xml")
	if err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("cannot read container.xml: %v", err)
	}

	var container xmlContainer
	if err := xml.Unmarshal(containerData, &container); err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("cannot parse container.xml: %v", err)
	}
	if len(container.Rootfiles.Rootfile) == 0 {
		zr.Close()
		return nil, nil, fmt.Errorf("no rootfile declared in container.xml")
	}
	opfPath := container.Rootfiles.Rootfile[0].FullPath

	opfData, err := readZipFile(zr, opfPath)
	if err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("cannot read opf: %v", err)
	}

	var pkg opfPackage
	if err := xml.Unmarshal(opfData, &pkg); err != nil {
		zr.Close()
		return nil, nil, fmt.Errorf("cannot parse opf: %v", err)
	}

	opfDir := path.Dir(opfPath)
	readRel := func(href string) ([]byte, error) {
		return readZipFile(zr, resolveZipPath(opfDir, href))
	}

	info, coverHref := finalizeOpfInfo(&pkg, readRel)

	var specified coverPick
	if coverHref != "" {
		specified = coverPick{internalName: resolveZipPath(opfDir, coverHref)}
	}
	findInternal := func(name string) (string, bool) {
		p := resolveZipPath(opfDir, name)
		if findZipFile(zr, p) != nil {
			return p, true
		}
		return "", false
	}
	pick := resolveCover(specified, findInternal, filepath.Dir(epubPath), nil)
	cover := coverSourceFrom(pick, func(name, dest string) error {
		data, err := readZipFile(zr, name)
		if err != nil {
			return err
		}
		return os.WriteFile(dest, data, 0644)
	}, zr.Close)

	return info, cover, nil
}

func filterInfo(full map[string]interface{}, requested []string) map[string]interface{} {
	out := map[string]interface{}{}
	for _, k := range requested {
		if v, ok := full[k]; ok {
			out[k] = v
		} else {
			out[k] = map[string]interface{}{"value": ""}
		}
	}
	return out
}


var volumeRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[vV](?:ol)?\.?\s*(\d+(?:\.\d+)?(?:\s*[-–—]\s*\d+(?:\.\d+)?)?)`),
	regexp.MustCompile(`(?i)\b[Vv]olume\.?\s*(\d+(?:\.\d+)?(?:\s*[-–—]\s*\d+(?:\.\d+)?)?)`),
	regexp.MustCompile(`(?i)\b[Tt](?:ome?)?\.?\s*(\d+(?:\.\d+)?(?:\s*[-–—]\s*\d+(?:\.\d+)?)?)`),
	regexp.MustCompile(`(?i)\b[Ss]0?(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)卷\s*(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)册\s*(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*巻`),
	regexp.MustCompile(`(?i)권\s*(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*권`),
	regexp.MustCompile(`(?i)[Тт]ом(?:а)?\s*(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)เล่ม(?:ที่)?\s*(\d+(?:\.\d+)?)`),
}

var chapterRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b[cC](?:h(?:apter)?|hp)?\.?\s*(\d+(?:\.\d+)?[xX]?(?:\s*[-–—]\s*\d+(?:\.\d+)?[xX]?)?)`),
	regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*話`),
	regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*话`),
	regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*[화회]`),
	regexp.MustCompile(`(?i)[บทที่ตอนที่]\s*(\d+(?:\.\d+)?)`),
	regexp.MustCompile(`(?i)[Гг]лава\s*(\d+(?:\.\d+)?)`),
}

var bareNumberRe = regexp.MustCompile(`(?i)(?:^|[-_\s])(\d+(?:\.\d+)?[xX]?)(?:\s*[-–—]\s*\d+(?:\.\d+)?[xX]?)?(?:$|[-_\s])`)

func normalizeRange(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "–", "-")
	s = strings.ReplaceAll(s, "—", "-")
	return s
}

func parseFilenameMetadata(name string) (volume, numbers string) {
	name = strings.TrimSpace(name)
	for _, re := range volumeRes {
		if m := re.FindStringSubmatch(name); m != nil {
			volume = normalizeRange(m[1])
			break
		}
	}
	for _, re := range chapterRes {
		if m := re.FindStringSubmatch(name); m != nil {
			numbers = normalizeRange(m[1])
			return
		}
	}
	search := name
	if volume != "" {
		for _, re := range volumeRes {
			if loc := re.FindStringIndex(name); loc != nil {
				search = name[:loc[0]] + strings.Repeat(" ", loc[1]-loc[0]) + name[loc[1]:]
				break
			}
		}
	}
	if m := bareNumberRe.FindStringSubmatch(search); m != nil {
		numbers = normalizeRange(m[1])
	}
	return
}

type comicInfoRawElement struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type comicInfoPage struct {
	Image int    `xml:"Image,attr"`
	Type  string `xml:"Type,attr"`
}

type comicInfoXML struct {
	XMLName xml.Name `xml:"ComicInfo"`
	Pages   struct {
		Page []comicInfoPage `xml:"Page"`
	} `xml:"Pages"`
	Items []comicInfoRawElement `xml:",any"`
}

var comicImageExtensions = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true, ".tif": true, ".tiff": true,
}

func isComicImageName(name string) bool {
	base := path.Base(name)
	if base == "" || strings.HasPrefix(base, ".") {
		return false
	}
	if strings.Contains(name, "__MACOSX") {
		return false
	}
	return comicImageExtensions[strings.ToLower(path.Ext(base))]
}

func isCoverLikeName(name string) bool {
	base := path.Base(name)
	stem := strings.TrimSuffix(base, path.Ext(base))
	return strings.EqualFold(stem, "cover")
}

func naturalLess(a, b string) bool {
	ai, bi := 0, 0
	for ai < len(a) && bi < len(b) {
		ca, cb := a[ai], b[bi]
		if isASCIIDigit(ca) && isASCIIDigit(cb) {
			sa := ai
			for ai < len(a) && isASCIIDigit(a[ai]) {
				ai++
			}
			sb := bi
			for bi < len(b) && isASCIIDigit(b[bi]) {
				bi++
			}
			na := strings.TrimLeft(a[sa:ai], "0")
			nb := strings.TrimLeft(b[sb:bi], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			continue
		}
		if ca != cb {
			return ca < cb
		}
		ai++
		bi++
	}
	return len(a)-ai < len(b)-bi
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

func sortComicImageNames(names []string) {
	sort.SliceStable(names, func(i, j int) bool {
		return naturalLess(names[i], names[j])
	})
}

func countStoryPages(imageNames []string, coverRef string, coverIsLoose bool) int {
	n := len(imageNames)
	if !coverIsLoose && coverRef != "" && isCoverLikeName(coverRef) {
		n--
	}
	if n < 0 {
		n = 0
	}
	return n
}

func addComicStr(info map[string]interface{}, key, val string) {
	if strings.TrimSpace(val) == "" {
		return
	}
	info[key] = map[string]interface{}{"value": val}
}

func parseComicInfo(data []byte) (*comicInfoXML, map[string]string) {
	if len(data) == 0 {
		return nil, nil
	}
	var ci comicInfoXML
	if err := xml.Unmarshal(data, &ci); err != nil {
		return nil, nil
	}
	generic := map[string]string{}
	for _, item := range ci.Items {
		val := strings.TrimSpace(item.Value)
		if val == "" {
			continue
		}
		generic[strings.ToLower(item.XMLName.Local)] = val
	}
	return &ci, generic
}

func buildComicInfo(generic map[string]string) map[string]interface{} {
	info := map[string]interface{}{}
	consumed := map[string]bool{}

	if year := generic["year"]; year != "" {
		date := year
		if m, err := strconv.Atoi(strings.TrimSpace(generic["month"])); err == nil && m > 0 {
			date = fmt.Sprintf("%s-%02d", date, m)
			if d, err := strconv.Atoi(strings.TrimSpace(generic["day"])); err == nil && d > 0 {
				date = fmt.Sprintf("%s-%02d", date, d)
			}
		}
		setCanonicalStr(info, "date", date)
		consumed["year"], consumed["month"], consumed["day"] = true, true, true
	}

	if series := strings.TrimSpace(generic["series"]); series != "" {
		var extra map[string]interface{}
		if sort := strings.TrimSpace(generic["seriessort"]); sort != "" {
			extra = map[string]interface{}{"ordered": sort}
		}
		setValue(info, "meta:series", series, extra)
		consumed["series"], consumed["seriessort"] = true, true
	}

	for k, v := range generic {
		if consumed[k] {
			continue
		}
		if setCanonicalStr(info, k, v) {
			continue
		}
		addComicStr(info, "meta:comicinfo:"+k, v)
	}

	return info
}

func comicInfoCoverIndex(ci *comicInfoXML) (int, bool) {
	if ci == nil {
		return -1, false
	}
	for _, p := range ci.Pages.Page {
		for _, t := range strings.Fields(p.Type) {
			if t == "FrontCover" {
				return p.Image, true
			}
		}
	}
	return -1, false
}

func findImageByName(imageNames []string, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	refSlash := filepath.ToSlash(ref)
	for _, n := range imageNames {
		if n == refSlash {
			return n, true
		}
	}
	refBase := path.Base(refSlash)
	for _, n := range imageNames {
		if strings.EqualFold(path.Base(n), refBase) {
			return n, true
		}
	}
	return "", false
}

func findCompanionComicInfo(dir string) ([]byte, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.EqualFold(e.Name(), "ComicInfo.xml") {
			if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				return data, true
			}
		}
	}
	return nil, false
}

func resolveComicSpecifiedCover(ci *comicInfoXML, generic map[string]string, imageNames []string) string {
	if ref, ok := generic["coverimage"]; ok {
		if name, found := findImageByName(imageNames, ref); found {
			return name
		}
	}
	if idx, ok := comicInfoCoverIndex(ci); ok && idx >= 0 && idx < len(imageNames) {
		return imageNames[idx]
	}
	return ""
}

func buildComicInfoResult(comicInfoData []byte, imageNames []string, filename string) (info map[string]interface{}, specified coverPick, declaredPages int) {
	ci, generic := parseComicInfo(comicInfoData)

	info = map[string]interface{}{}
	if generic != nil {
		info = buildComicInfo(generic)
	}
	volParsed, numParsed := parseFilenameMetadata(filename)
	if volParsed != "" && generic["volume"] == "" {
		setCanonicalStr(info, "volume", volParsed)
	}
	if numParsed != "" && generic["number"] == "" {
		setCanonicalStr(info, "number", numParsed)
	}

	if name := resolveComicSpecifiedCover(ci, generic, imageNames); name != "" {
		specified = coverPick{internalName: name}
	}

	if pc, ok := generic["pagecount"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(pc)); err == nil && n > 0 {
			declaredPages = n
		}
	}

	return info, specified, declaredPages
}

func loadComicArchive(archivePath string) (map[string]interface{}, *coverSource, error) {
	fsys, err := archives.FileSystem(context.Background(), archivePath, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot open %s: %v", filepath.Base(archivePath), err)
	}

	dir := filepath.Dir(archivePath)
	externalData, fromParent := findCompanionComicInfo(dir)

	var internalData []byte
	var imageNames []string

	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.EqualFold(path.Base(p), "ComicInfo.xml") {
			if !fromParent {
				if f, oerr := fsys.Open(p); oerr == nil {
					internalData, _ = io.ReadAll(f)
					f.Close()
				}
			}
			return nil // nunca cuenta como imagen de página, tampoco
		}
		if isComicImageName(p) {
			imageNames = append(imageNames, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read archive: %v", err)
	}
	sortComicImageNames(imageNames)

	comicInfoData := internalData
	if fromParent {
		comicInfoData = externalData
	}

	filename := strings.TrimSuffix(filepath.Base(archivePath), filepath.Ext(archivePath))
	info, specified, declaredPages := buildComicInfoResult(comicInfoData, imageNames, filename)
	if fromParent {
		info["from_parent"] = map[string]interface{}{"value": true}
	}

	findInternal := func(name string) (string, bool) {
		for _, n := range imageNames {
			if strings.EqualFold(path.Base(n), name) {
				return n, true
			}
		}
		return "", false
	}
	pick := resolveCover(specified, findInternal, dir, func() string {
		if len(imageNames) > 0 {
			return imageNames[0]
		}
		return ""
	})

	pageCount := declaredPages
	if pageCount == 0 {
		// PageCount no fue declarado (o vino inválido/cero): contamos las
		// imágenes nosotros mismos, descontando la portada solo cuando
		// hay evidencia concreta de que es un asset separado y dedicado
		// (un archivo "cover.*" realmente presente entre las imágenes).
		pageCount = countStoryPages(imageNames, pick.internalName, pick.loosePath != "")
	}
	if pageCount > 0 {
		pagesKey := canonicalFields["pagecount"]
		setValue(info, pagesKey, pageCount, canonicalExtra(pagesKey))
	}

	cover := coverSourceFrom(pick, func(name, dest string) error {
		f, err := fsys.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		out, err := os.Create(dest)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, f)
		return err
	}, func() error { return nil })

	return info, cover, nil
}


type PDFPermissions struct {
	Print            bool `json:"print"`
	Modify           bool `json:"modify"`
	Copy             bool `json:"copy"`
	Annotations      bool `json:"annotations"`
	FillForms        bool `json:"fill_forms"`
	Accessibility    bool `json:"accessibility"`
	Assembly         bool `json:"assembly"`
	HighQualityPrint bool `json:"high_quality_print"`
}

var pdfAllPermissions = PDFPermissions{
	Print: true, Modify: true, Copy: true, Annotations: true,
	FillForms: true, Accessibility: true, Assembly: true, HighQualityPrint: true,
}

func parsePDFPermissions(p int32) PDFPermissions {
	up := uint32(p)
	return PDFPermissions{
		Print:            (up & (1 << 2)) != 0,
		Modify:           (up & (1 << 3)) != 0,
		Copy:             (up & (1 << 4)) != 0,
		Annotations:      (up & (1 << 5)) != 0,
		FillForms:        (up & (1 << 8)) != 0,
		Accessibility:    (up & (1 << 9)) != 0,
		Assembly:         (up & (1 << 10)) != 0,
		HighQualityPrint: (up & (1 << 11)) != 0,
	}
}

var pdfDateRe = regexp.MustCompile(`^D:(\d{4})(\d{2})?(\d{2})?(\d{2})?(\d{2})?(\d{2})?([Zz+-])?(\d{2})?'?(\d{2})?'?`)

func parsePDFDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	m := pdfDateRe.FindStringSubmatch(raw)
	if m == nil {
		return raw
	}
	get := func(i int, def string) string {
		if i < len(m) && m[i] != "" {
			return m[i]
		}
		return def
	}
	year, month, day := get(1, "0001"), get(2, "01"), get(3, "01")
	hour, minute, sec := get(4, "00"), get(5, "00"), get(6, "00")
	tzSign := get(7, "Z")
	offset := "Z"
	if tzSign == "+" || tzSign == "-" {
		offset = tzSign + get(8, "00") + ":" + get(9, "00")
	}
	return fmt.Sprintf("%s-%s-%sT%s:%s:%s%s", year, month, day, hour, minute, sec, offset)
}


const rdfNS = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"

var xmpNamespacePrefixes = map[string]string{
	"http://ns.adobe.com/pdf/1.3/":                   "pdf",
	"http://ns.adobe.com/xap/1.0/":                   "xmp",
	"http://ns.adobe.com/xap/1.0/mm/":                "xmpmm",
	"http://ns.adobe.com/xap/1.0/rights/":            "xmprights",
	"http://ns.adobe.com/photoshop/1.0/":             "photoshop",
	"http://purl.org/dc/terms/":                      "dcterms",
	"http://ns.adobe.com/pdfx/1.3/":                  "pdfx",
	"http://prismstandard.org/namespaces/basic/2.0/": "prism",
}

func setXMPValue(info map[string]interface{}, ns, local, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}

	// CreatorTool/Producer have no unified equivalent — they always live
	// under their own fixed PDF-specific key, the same one the Info dict
	// uses, regardless of which XMP schema they arrived under.
	switch local {
	case "CreatorTool":
		setValue(info, "meta:pdf:CreatorTool", val, nil)
		return
	case "Producer":
		setValue(info, "meta:pdf:Producer", val, nil)
		return
	}

	// Everything else — CreateDate/ModifyDate/Keywords, dc:* elements,
	// pdfx:ISBN/prism:ISBN, ... — goes through the same canonical table
	// every other parser uses, matched purely on the property's local
	// name regardless of namespace.
	if setCanonicalStr(info, local, val) {
		return
	}

	if ns == dcNS {
		setValue(info, "dc:"+local, val, nil)
		return
	}
	prefix, ok := xmpNamespacePrefixes[ns]
	if !ok {
		prefix = "xmp"
	}
	setValue(info, "meta:"+prefix+":"+local, val, nil)
}

func parseXMPMetadata(info map[string]interface{}, xmpData []byte) {
	decoder := xml.NewDecoder(bytes.NewReader(xmpData))
	for {
		tok, err := decoder.Token()
		if err != nil {
			return
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "Description" || start.Name.Space != rdfNS {
			continue
		}
		for _, a := range start.Attr {
			if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
				continue
			}
			setXMPValue(info, a.Name.Space, a.Name.Local, a.Value)
		}
		parseXMPDescriptionBody(info, decoder)
	}
}

func parseXMPDescriptionBody(info map[string]interface{}, decoder *xml.Decoder) {
	var propSpace, propLocal string
	var items []string
	var current strings.Builder
	depth := 0

	flushItem := func() {
		s := strings.TrimSpace(current.String())
		if s != "" {
			items = append(items, s)
		}
		current.Reset()
	}

	for {
		tok, err := decoder.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if t.Name.Space == rdfNS {
					continue
				}
				propSpace, propLocal = t.Name.Space, t.Name.Local
				items = nil
				current.Reset()
				depth = 1
				continue
			}
			depth++
			if t.Name.Space == rdfNS && t.Name.Local == "li" {
				flushItem()
			}
		case xml.CharData:
			if depth > 0 {
				current.WriteString(string(t))
			}
		case xml.EndElement:
			if t.Name.Space == rdfNS && t.Name.Local == "Description" {
				return
			}
			if depth == 0 {
				continue
			}
			if t.Name.Space == rdfNS && t.Name.Local == "li" {
				flushItem()
				depth--
				continue
			}
			depth--
			if depth == 0 {
				flushItem()
				val := strings.Join(items, "; ")
				if val != "" {
					setXMPValue(info, propSpace, propLocal, val)
				}
			}
		}
	}
}


func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\uFEFF")
}

func readPDFPageTree(ctx *model.Context) (count int, width, height float64) {
	if ctx.Root == nil {
		return 0, 0, 0
	}
	rootObj, err := ctx.XRefTable.Dereference(*ctx.Root)
	if err != nil {
		return 0, 0, 0
	}
	rootDict, ok := rootObj.(types.Dict)
	if !ok {
		return 0, 0, 0
	}

	pagesRef, ok := rootDict["Pages"].(types.IndirectRef)
	if !ok {
		return 0, 0, 0
	}

	pagesObj, err := ctx.XRefTable.Dereference(pagesRef)
	if err != nil {
		return 0, 0, 0
	}
	pagesDict, ok := pagesObj.(types.Dict)
	if !ok {
		return 0, 0, 0
	}

	count = intFromDict(ctx, pagesDict, "Count")

	w, h := walkPageTreeForMediaBox(ctx, pagesRef, 0)
	return count, w, h
}

func walkPageTreeForMediaBox(ctx *model.Context, ref types.IndirectRef, depth int) (float64, float64) {
	if depth > 50 {
		return 0, 0
	}
	obj, err := ctx.XRefTable.Dereference(ref)
	if err != nil {
		return 0, 0
	}
	dict, ok := obj.(types.Dict)
	if !ok {
		return 0, 0
	}

	typeObj, _ := ctx.XRefTable.Dereference(dict["Type"])
	typeName, _ := typeObj.(types.Name)
	if typeName.String() == "Page" {
		return mediaBoxFromDict(ctx, dict)
	}

	kidsObj, _ := ctx.XRefTable.Dereference(dict["Kids"])
	kids, ok := kidsObj.(types.Array)
	if !ok {
		return 0, 0
	}

	for _, kid := range kids {
		kidRef, ok := kid.(types.IndirectRef)
		if !ok {
			continue
		}
		w, h := walkPageTreeForMediaBox(ctx, kidRef, depth+1)
		if w > 0 && h > 0 {
			return w, h
		}
	}
	return 0, 0
}

func mediaBoxFromDict(ctx *model.Context, dict types.Dict) (float64, float64) {
	mbObj, _ := ctx.XRefTable.Dereference(dict["MediaBox"])
	arr, ok := mbObj.(types.Array)
	if !ok || len(arr) < 4 {
		return 0, 0
	}
	vals := make([]float64, 4)
	for i := 0; i < 4; i++ {
		switch v := arr[i].(type) {
		case types.Integer:
			vals[i] = float64(v.Value())
		case types.Float:
			vals[i] = float64(v.Value())
		default:
			return 0, 0
		}
	}
	return vals[2] - vals[0], vals[3] - vals[1]
}

func intFromDict(ctx *model.Context, dict types.Dict, key string) int {
	obj, _ := ctx.XRefTable.Dereference(dict[key])
	if v, ok := obj.(types.Integer); ok {
		return v.Value()
	}
	return 0
}

func loadPdf(pdfPath string) (map[string]interface{}, *coverSource, error) {
	conf := model.NewDefaultConfiguration()

	ctx, err := pdfcpu.ReadFile(pdfPath, conf)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read pdf: %v", err)
	}

	info := map[string]interface{}{}

	// ---- version ----
	if ctx.HeaderVersion != nil {
		info["meta:pdf:version"] = map[string]interface{}{"value": ctx.HeaderVersion.String()}
	}

	// ---- encryption & permissions ----
	isEncrypted := ctx.Encrypt != nil
	info["meta:pdf:is_encrypted"] = map[string]interface{}{"value": isEncrypted, "type": "bool"}

	var perms PDFPermissions
	if isEncrypted && ctx.E != nil {
		perms = parsePDFPermissions(int32(ctx.E.P))
	} else {
		perms = pdfAllPermissions
	}

	setPerm := func(key string, val bool) {
		info[key] = map[string]interface{}{"value": val, "type": "bool"}
	}
	setPerm("meta:pdf:perm_print", perms.Print)
	setPerm("meta:pdf:perm_modify", perms.Modify)
	setPerm("meta:pdf:perm_copy", perms.Copy)
	setPerm("meta:pdf:perm_annotations", perms.Annotations)
	setPerm("meta:pdf:perm_fill_forms", perms.FillForms)
	setPerm("meta:pdf:perm_accessibility", perms.Accessibility)
	setPerm("meta:pdf:perm_assembly", perms.Assembly)
	setPerm("meta:pdf:perm_high_quality_print", perms.HighQualityPrint)

	readInfoString := func(d types.Dict, key string) string {
		obj, ok := d[key]
		if !ok {
			return ""
		}
		s, err := ctx.DereferenceText(obj)
		if err != nil {
			return ""
		}
		return stripBOM(s)
	}

	// setRaw stores a PDF-specific value that has no unified equivalent
	// (CreatorTool, Producer, custom Info-dict properties, ...) directly
	// under its own key, with the same trim/skip-empty behavior as
	// setCanonicalStr.
	setRaw := func(key, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		setValue(info, key, val, nil)
	}

	// ---- Info dict
	if ctx.Info != nil {
		if d, err := ctx.DereferenceDict(*ctx.Info); err == nil && d != nil {
			setCanonicalStr(info, "title", readInfoString(d, "Title"))
			setCanonicalStr(info, "author", readInfoString(d, "Author"))
			setCanonicalStr(info, "description", readInfoString(d, "Subject"))
			setRaw("meta:pdf:CreatorTool", readInfoString(d, "Creator"))
			setRaw("meta:pdf:Producer", readInfoString(d, "Producer"))
			setCanonicalStr(info, "creationdate", parsePDFDate(readInfoString(d, "CreationDate")))
			setCanonicalStr(info, "moddate", parsePDFDate(readInfoString(d, "ModDate")))
			setCanonicalStr(info, "keywords", readInfoString(d, "Keywords"))

			// Every other Info-dict entry (custom metadata) is tried
			// against the same canonical table first — a publisher could
			// just as easily have stuffed e.g. "ISBN" in here — and
			// anything unrecognized falls back to
			// meta:pdf:properties:<Key> so nothing is lost.
			for key := range d {
				switch key {
				case "Title", "Author", "Subject", "Keywords",
					"Creator", "Producer", "CreationDate", "ModDate":
					continue
				}
				val := readInfoString(d, key)
				if setCanonicalStr(info, key, val) {
					continue
				}
				setRaw("meta:pdf:properties:"+key, val)
			}
		}
	}

	// ---- pages & paper size (manual page-tree walk, avoids pdfcpu bugs) ----
	pageCount, pageW, pageH := readPDFPageTree(ctx)
	if pageCount > 0 {
		setValue(info, canonicalFields["pagecount"], pageCount, canonicalExtra(canonicalFields["pagecount"]))
	}
	if pageW > 0 && pageH > 0 {
		info["meta:pdf:paper_width"] = map[string]interface{}{"value": pageW, "type": "number"}
		info["meta:pdf:paper_height"] = map[string]interface{}{"value": pageH, "type": "number"}
		info["meta:pdf:paper_size"] = map[string]interface{}{"value": fmt.Sprintf("%.2f x %.2f", pageW, pageH), "type": "string"}
		wcm := math.Round(pageW/28.35*100) / 100
		hcm := math.Round(pageH/28.35*100) / 100
		info["meta:pdf:paper_width_cm"] = map[string]interface{}{"value": wcm, "type": "double"}
		info["meta:pdf:paper_height_cm"] = map[string]interface{}{"value": hcm, "type": "double"}
		info["meta:pdf:paper_size_cm"] = map[string]interface{}{"value": fmt.Sprintf("%.2f x %.2f cm", wcm, hcm), "type": "string"}
		win := math.Round(pageW/72*100) / 100
		hin := math.Round(pageH/72*100) / 100
		info["meta:pdf:paper_width_in"] = map[string]interface{}{"value": win, "type": "double"}
		info["meta:pdf:paper_height_in"] = map[string]interface{}{"value": hin, "type": "double"}
		info["meta:pdf:paper_size_in"] = map[string]interface{}{"value": fmt.Sprintf("%.2f x %.2f in", win, hin), "type": "string"}
	}

	metaStreams, err := pdfcpu.ExtractMetadata(ctx)
	if err == nil {
		for _, meta := range metaStreams {
			data, err := io.ReadAll(meta)
			if err == nil && len(data) > 0 {
				parseXMPMetadata(info, data)
			}
		}
	}

	pick := resolveCover(coverPick{}, nil, filepath.Dir(pdfPath), nil)
	cover := coverSourceFrom(pick, nil, func() error { return nil })

	return info, cover, nil
}

func stripObjectValues(info map[string]interface{}) {
	for k, v := range info {
		entry, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		if _, isObject := entry["value"].(map[string]interface{}); isObject {
			delete(info, k)
		}
	}
}

func writeError(msg string) []byte {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

func writeJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return writeError("json marshal error: " + err.Error())
	}
	return b
}

func sendJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func handleCloseServer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendJSON(w, http.StatusMethodNotAllowed, writeError("method not allowed, use GET"))
		return
	}

	logf("close_server received, shutting down immediately.")
	sendJSON(w, http.StatusOK, []byte(`{"status":"closing"}`))

	go func() {
		time.Sleep(50 * time.Millisecond)
		os.Remove(socketPath)
		os.Exit(0)
	}()
}

type getMetaAllRequest struct {
	File    string `json:"file"`
	DoCover bool   `json:"do_cover"`
}

func handleGetMetaAll(w http.ResponseWriter, r *http.Request) {
	touchActivity()

	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, writeError("method not allowed, use POST"))
		return
	}

	var req getMetaAllRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSON(w, http.StatusOK, writeError("invalid JSON body: "+err.Error()))
			return
		}
	}
	if strings.TrimSpace(req.File) == "" {
		sendJSON(w, http.StatusOK, writeError("missing required field: file"))
		return
	}

	info, cover, err := loadEpub(req.File)
	if err != nil {
		sendJSON(w, http.StatusOK, writeError(err.Error()))
		return
	}
	defer cover.close()

	coverOut := ""
	if req.DoCover {
		coverOut = cover.materializeToTemp()
	}

	sendJSON(w, http.StatusOK, writeJSON(map[string]interface{}{"info": info, "cover": coverOut}))
}

type getMetaRequest struct {
	File string   `json:"file"`
	Info []string `json:"info"`
}

func handleGetMeta(w http.ResponseWriter, r *http.Request) {
	touchActivity()

	if r.Method != http.MethodPost {
		sendJSON(w, http.StatusMethodNotAllowed, writeError("method not allowed, use POST"))
		return
	}

	var req getMetaRequest
	if r.Body != nil {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			sendJSON(w, http.StatusOK, writeError("invalid JSON body: "+err.Error()))
			return
		}
	}
	if strings.TrimSpace(req.File) == "" {
		sendJSON(w, http.StatusOK, writeError("missing required field: file"))
		return
	}

	info, cover, err := loadEpub(req.File)
	if err != nil {
		sendJSON(w, http.StatusOK, writeError(err.Error()))
		return
	}
	cover.close()

	sendJSON(w, http.StatusOK, writeJSON(map[string]interface{}{"info": filterInfo(info, req.Info)}))
}

func routeHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/get_meta_all":
		handleGetMetaAll(w, r)
	case "/get_meta":
		handleGetMeta(w, r)
	case "/close_server":
		handleCloseServer(w, r)
	default:
		http.NotFound(w, r)
	}
}

type unixResponseWriter struct {
	conn        net.Conn
	headers     http.Header
	statusCode  int
	buf         *bytes.Buffer
	wroteHeader bool
}

func (w *unixResponseWriter) Header() http.Header { return w.headers }

func (w *unixResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.statusCode = code
}

func (w *unixResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.buf.Write(b)
}

func (w *unixResponseWriter) flush() {
	code := w.statusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := w.buf.Bytes()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", code, http.StatusText(code)))
	for k, vs := range w.headers {
		for _, v := range vs {
			sb.WriteString(textproto.CanonicalMIMEHeaderKey(k))
			sb.WriteString(": ")
			sb.WriteString(v)
			sb.WriteString("\r\n")
		}
	}
	if _, hasLen := w.headers["Content-Length"]; !hasLen {
		sb.WriteString(fmt.Sprintf("Content-Length: %d\r\n", len(body)))
	}
	sb.WriteString("Connection: close\r\n")
	sb.WriteString("\r\n")
	w.conn.Write([]byte(sb.String()))
	w.conn.Write(body)
}

func handleUnixConn(conn net.Conn, mux *http.ServeMux) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				logf("read request error: %v", err)
			}
			return
		}
		if req.Host == "" {
			req.Host = "localhost"
		}
		rw := &unixResponseWriter{
			conn:    conn,
			headers: make(http.Header),
			buf:     &bytes.Buffer{},
		}
		mux.ServeHTTP(rw, req)
		rw.flush()
		if req.Close {
			return
		}
	}
}

func serveUnix(ln net.Listener, mux *http.ServeMux) {
	for {
		select {
		case <-shutdownCh:
			return
		default:
		}

		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-shutdownCh:
				return
			default:
				logf("accept error: %v", err)
			}
			return
		}
		go handleUnixConn(conn, mux)
	}
}

func watchdogLogic() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if awakeSecs <= 0 {
				continue
			}
			lastActivityMu.RLock()
			la := lastActivity
			lastActivityMu.RUnlock()

			if time.Since(la).Seconds() >= float64(awakeSecs) {
				logf("WATCHDOG: idle timeout reached, closing server...")
				os.Remove(socketPath)
				os.Exit(0)
			}
		case <-shutdownCh:
			return
		}
	}
}

func isServerAlive(path string) bool {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false
	}
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[goebook] "+format+"\n", args...)
}

func main() {
	argsRaw := os.Args[1:]
	argsDict := map[string]string{}
	for _, a := range argsRaw {
		if idx := strings.Index(a, "="); idx != -1 {
			key := strings.ToUpper(strings.TrimLeft(a[:idx], "-"))
			argsDict[key] = a[idx+1:]
		}
	}

	if epubPath, ok := argsDict["GETMETA_ALL"]; ok {
		info, cover, err := loadEpub(epubPath)
		if err != nil {
			fmt.Println(string(writeError(err.Error())))
			os.Exit(1)
		}
		coverOut := cover.materializeToTemp()
		cover.close()
		fmt.Println(string(writeJSON(map[string]interface{}{"info": info, "cover": coverOut})))
		return
	}

	if epubPath, ok := argsDict["COVER"]; ok {
		out, hasOut := argsDict["OUT"]
		if !hasOut || strings.TrimSpace(out) == "" {
			fmt.Println(string(writeError("missing required argument: -OUT")))
			os.Exit(1)
		}

		_, cover, err := loadEpub(epubPath)
		if err != nil {
			fmt.Println(string(writeError(err.Error())))
			os.Exit(1)
		}

		defer cover.close()

		if err := cover.materialize(out); err != nil {
			fmt.Println(string(writeError(err.Error())))
			os.Exit(1)
		}

		fmt.Println(string(writeJSON(map[string]interface{}{"cover": out})))
		return
	}

	sp, hasSocket := argsDict["SOCKET"]
	awakeArg, hasAwake := argsDict["AWAKE"]
	if !hasSocket || !hasAwake || strings.TrimSpace(sp) == "" {
		return
	}
	socketPath = sp

	n, err := strconv.Atoi(strings.TrimSpace(awakeArg))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid AWAKE value: %v\n", awakeArg)
		os.Exit(1)
	}
	awakeSecs = n

	if isServerAlive(socketPath) {
		logf("Server already running on %s, exiting.", socketPath)
		os.Exit(0)
	}

	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		logf("ERROR starting listener: %v", err)
		os.Exit(1)
	}
	defer os.Remove(socketPath)
	logf("Listener bound to: %s", socketPath)

	touchActivity()

	mux := http.NewServeMux()
	mux.HandleFunc("/", routeHandler)

	go watchdogLogic()
	go serveUnix(ln, mux)

	logf("Server init on socket: %s awake: %ds", socketPath, awakeSecs)

	<-shutdownCh
	os.Remove(socketPath)
	logf("Main loop exiting")
}