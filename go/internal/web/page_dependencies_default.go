//go:build !e2e

package web

import "net/http"

func resolvePageDependencies(_ *http.Request, base pageDependencies) (pageDependencies, string, bool) {
	return base, "", true
}
