package codex

import (
	"runtime"
	"strings"
)

var builtInSecretEnvironmentNames = []string{"GH_TOKEN", "GITHUB_TOKEN", "LINEAR_API_KEY"}

// SanitizeEnvironment returns a detached child environment with every known
// tracker credential removed. Duplicate safe keys use the platform's normal
// last-value-wins behavior.
func SanitizeEnvironment(environment, declaredSecretNames []string) []string {
	return sanitizeEnvironmentForOS(environment, declaredSecretNames, runtime.GOOS)
}

func sanitizeEnvironmentForOS(environment, declaredSecretNames []string, goos string) []string {
	secretNames := make(map[string]struct{}, len(builtInSecretEnvironmentNames)+len(declaredSecretNames))
	for _, supplied := range append(append([]string(nil), builtInSecretEnvironmentNames...), declaredSecretNames...) {
		if name, ok := normalizeSecretEnvironmentName(supplied); ok {
			secretNames[strings.ToLower(name)] = struct{}{}
		}
	}

	type entry struct {
		key       string
		dedupeKey string
		value     string
		index     int
	}
	entries := make([]entry, 0, len(environment))
	last := make(map[string]int, len(environment))
	for index, raw := range append([]string(nil), environment...) {
		key, value, ok := splitEnvironmentEntry(raw)
		if !ok {
			continue
		}
		if _, sensitive := secretNames[strings.ToLower(key)]; sensitive {
			continue
		}
		dedupeKey := key
		if goos == "windows" {
			dedupeKey = strings.ToLower(key)
		}
		entries = append(entries, entry{key: key, dedupeKey: dedupeKey, value: value, index: index})
		last[dedupeKey] = index
	}

	result := make([]string, 0, len(entries))
	for _, item := range entries {
		if last[item.dedupeKey] != item.index {
			continue
		}
		result = append(result, item.key+"="+item.value)
	}
	return result
}

func splitEnvironmentEntry(raw string) (string, string, bool) {
	separator := strings.IndexByte(raw, '=')
	if separator <= 0 || strings.IndexByte(raw, 0) >= 0 {
		return "", "", false
	}
	return raw[:separator], raw[separator+1:], true
}

func normalizeSecretEnvironmentName(supplied string) (string, bool) {
	name := supplied
	if strings.HasPrefix(name, "$") {
		name = name[1:]
	}
	if name == "" || !isEnvironmentNameStart(name[0]) {
		return "", false
	}
	for index := 1; index < len(name); index++ {
		if !isEnvironmentNamePart(name[index]) {
			return "", false
		}
	}
	return name, true
}

func isEnvironmentNameStart(value byte) bool {
	return value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isEnvironmentNamePart(value byte) bool {
	return isEnvironmentNameStart(value) || value >= '0' && value <= '9'
}
