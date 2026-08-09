package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/coryj627/symphony/go/internal/tracker"
)

const maxPages = 100

type cachedPage struct {
	valid bool
	etag  string
	body  []byte
	links []string
}

type statePage struct {
	number int
	body   []byte
}

func (adapter *Adapter) fetchStatePages(ctx context.Context) ([]statePage, map[string]cachedPage, error) {
	pages := make([]statePage, 0, 1)
	staged := make(map[string]cachedPage)
	visitedPages := make(map[int]struct{})
	visitedURLs := make(map[string]struct{})
	pageNumber := 1

	for {
		if len(pages) >= maxPages || pageNumber < 1 || pageNumber > maxPages {
			return nil, nil, paginationError("GitHub pagination exceeded the page limit")
		}
		requestURL := adapter.statePageURL(pageNumber)
		key := requestURL.String()
		if _, found := visitedPages[pageNumber]; found {
			return nil, nil, paginationError("GitHub pagination repeated a page")
		}
		if _, found := visitedURLs[key]; found {
			return nil, nil, paginationError("GitHub pagination repeated a request")
		}
		visitedPages[pageNumber] = struct{}{}
		visitedURLs[key] = struct{}{}

		cached, cachedExact := adapter.pageCache[key]
		etag := ""
		if cachedExact && cached.valid && cached.etag != "" {
			etag = cached.etag
		}
		response, err := adapter.request(ctx, requestURL, etag, true, false)
		if err != nil {
			return nil, nil, err
		}

		var page cachedPage
		switch response.status {
		case http.StatusOK:
			page = cachedPage{
				valid: true,
				etag:  response.header.Get("ETag"),
				body:  append([]byte(nil), response.body...),
				links: append([]string(nil), response.header.Values("Link")...),
			}
		case http.StatusNotModified:
			if !cachedExact || !cached.valid || cached.etag == "" || etag != cached.etag {
				return nil, nil, &tracker.Error{
					Category: tracker.CategoryResponse,
					Message:  "GitHub returned an unmatched not-modified response",
					Status:   http.StatusNotModified,
				}
			}
			page = cloneCachedPage(cached)
		default:
			return nil, nil, &tracker.Error{Category: tracker.CategoryResponse, Message: "GitHub returned an unexpected status", Status: response.status}
		}
		staged[key] = cloneCachedPage(page)
		pages = append(pages, statePage{number: pageNumber, body: append([]byte(nil), page.body...)})

		nextPage, found, err := nextPageFromLinks(page.links, requestURL, adapter.origin)
		if err != nil {
			return nil, nil, err
		}
		if !found {
			return pages, staged, nil
		}
		if len(pages) >= maxPages || nextPage > maxPages {
			return nil, nil, paginationError("GitHub pagination exceeded the page limit")
		}
		if _, found := visitedPages[nextPage]; found {
			return nil, nil, paginationError("GitHub pagination formed a cycle")
		}
		pageNumber = nextPage
	}
}

func cloneCachedPage(page cachedPage) cachedPage {
	return cachedPage{
		valid: page.valid,
		etag:  page.etag,
		body:  append([]byte(nil), page.body...),
		links: append([]string(nil), page.links...),
	}
}

func nextPageFromLinks(values []string, current *url.URL, allowed apiOrigin) (int, bool, error) {
	if current == nil {
		return 0, false, paginationError("GitHub pagination current page was invalid")
	}
	currentPage, err := strconv.Atoi(current.Query().Get("page"))
	if err != nil || currentPage < 1 || currentPage > maxPages {
		return 0, false, paginationError("GitHub pagination current page was invalid")
	}
	nextPage := 0
	foundNext := false
	for _, value := range values {
		parts, err := splitLinkValues(value)
		if err != nil {
			return 0, false, paginationError("GitHub pagination metadata was malformed")
		}
		for _, part := range parts {
			target, relations, err := parseLinkValue(part)
			if err != nil {
				return 0, false, paginationError("GitHub pagination metadata was malformed")
			}
			if !containsRelation(relations, "next") {
				continue
			}
			if foundNext {
				return 0, false, paginationError("GitHub pagination contained duplicate next links")
			}
			parsed, err := url.Parse(target)
			if err != nil {
				return 0, false, paginationError("GitHub pagination next link was invalid")
			}
			parsed = current.ResolveReference(parsed)
			if parsed.Fragment != "" || !sameHTTPSOrigin(parsed, allowed) {
				return 0, false, paginationError("GitHub pagination next link left the configured origin")
			}
			query, err := url.ParseQuery(parsed.RawQuery)
			if err != nil {
				return 0, false, paginationError("GitHub pagination next link had an invalid query")
			}
			pageValues, present := query["page"]
			if !present || len(pageValues) != 1 {
				return 0, false, paginationError("GitHub pagination next link had an invalid page")
			}
			pageText := pageValues[0]
			if pageText == "" || strings.HasPrefix(pageText, "+") || (len(pageText) > 1 && pageText[0] == '0') {
				return 0, false, paginationError("GitHub pagination next link had an invalid page")
			}
			page, err := strconv.Atoi(pageText)
			if err != nil || page < 1 || page > maxPages {
				return 0, false, paginationError("GitHub pagination next link had an invalid page")
			}
			if page != currentPage+1 {
				return 0, false, paginationError("GitHub pagination next link was not sequential")
			}
			nextPage = page
			foundNext = true
		}
	}
	return nextPage, foundNext, nil
}

func splitLinkValues(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("empty Link value")
	}
	var (
		parts   []string
		start   int
		inAngle bool
		inQuote bool
		escaped bool
	)
	for index, character := range value {
		if escaped {
			escaped = false
			continue
		}
		if inQuote && character == '\\' {
			escaped = true
			continue
		}
		switch character {
		case '"':
			if !inAngle {
				inQuote = !inQuote
			}
		case '<':
			if !inQuote {
				if inAngle {
					return nil, fmt.Errorf("nested angle bracket")
				}
				inAngle = true
			}
		case '>':
			if !inQuote {
				if !inAngle {
					return nil, fmt.Errorf("unmatched angle bracket")
				}
				inAngle = false
			}
		case ',':
			if !inAngle && !inQuote {
				part := strings.TrimSpace(value[start:index])
				if part == "" {
					return nil, fmt.Errorf("empty link")
				}
				parts = append(parts, part)
				start = index + 1
			}
		}
	}
	if inAngle || inQuote || escaped {
		return nil, fmt.Errorf("unterminated Link value")
	}
	last := strings.TrimSpace(value[start:])
	if last == "" {
		return nil, fmt.Errorf("empty link")
	}
	return append(parts, last), nil
}

func parseLinkValue(value string) (string, []string, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "<") {
		return "", nil, fmt.Errorf("missing target")
	}
	closing := strings.IndexByte(value, '>')
	if closing <= 1 {
		return "", nil, fmt.Errorf("invalid target")
	}
	target := value[1:closing]
	remainder := strings.TrimSpace(value[closing+1:])
	if remainder == "" {
		return target, nil, nil
	}
	if !strings.HasPrefix(remainder, ";") {
		return "", nil, fmt.Errorf("invalid parameters")
	}
	parameters, err := splitParameters(remainder[1:])
	if err != nil {
		return "", nil, err
	}
	var relations []string
	for _, parameter := range parameters {
		name, rawValue, found := strings.Cut(parameter, "=")
		if !found || strings.TrimSpace(name) == "" {
			return "", nil, fmt.Errorf("invalid parameter")
		}
		decoded, err := decodeParameterValue(strings.TrimSpace(rawValue))
		if err != nil {
			return "", nil, err
		}
		if strings.EqualFold(strings.TrimSpace(name), "rel") {
			relations = append(relations, strings.Fields(decoded)...)
		}
	}
	return target, relations, nil
}

func splitParameters(value string) ([]string, error) {
	var (
		parts   []string
		start   int
		inQuote bool
		escaped bool
	)
	for index, character := range value {
		if escaped {
			escaped = false
			continue
		}
		if inQuote && character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			inQuote = !inQuote
			continue
		}
		if character == ';' && !inQuote {
			part := strings.TrimSpace(value[start:index])
			if part == "" {
				return nil, fmt.Errorf("empty parameter")
			}
			parts = append(parts, part)
			start = index + 1
		}
	}
	if inQuote || escaped {
		return nil, fmt.Errorf("unterminated parameter")
	}
	last := strings.TrimSpace(value[start:])
	if last == "" {
		return nil, fmt.Errorf("empty parameter")
	}
	return append(parts, last), nil
}

func decodeParameterValue(value string) (string, error) {
	if strings.HasPrefix(value, `"`) {
		if !strings.HasSuffix(value, `"`) {
			return "", fmt.Errorf("unterminated quoted value")
		}
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", err
		}
		return decoded, nil
	}
	if value == "" || strings.ContainsAny(value, " \t\r\n\"") {
		return "", fmt.Errorf("invalid token value")
	}
	return value, nil
}

func containsRelation(relations []string, wanted string) bool {
	for _, relation := range relations {
		if strings.EqualFold(relation, wanted) {
			return true
		}
	}
	return false
}
