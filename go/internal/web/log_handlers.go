package web

import (
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/coryj627/symphony/go/internal/observability"
)

const (
	maximumLogQueryBytes = 256
	maximumLogMessage    = 8 << 10
	maximumLogFields     = 16 << 10
	maximumLogFieldDepth = 16
	maximumLogFieldItems = 1024
)

type logFilters struct {
	Query  string
	Level  string
	Before uint64
}

type logRecordView struct {
	Sequence         uint64 `json:"sequence"`
	DateTime         string `json:"at"`
	DisplayTime      string `json:"-"`
	Level            string `json:"level"`
	Message          string `json:"message"`
	Fields           string `json:"fields"`
	MessageTruncated bool   `json:"message_truncated"`
	FieldsTruncated  bool   `json:"fields_truncated"`
}

func (handler *PageHandler) logsHTML(w http.ResponseWriter, request *http.Request) {
	dependencies := handler.dependencies(request)
	filters := parseLogFilters(request.URL.Query())
	page, err := dependencies.logs.Query(request.Context(), observability.LogQuery{Search: filters.Query, Level: filters.Level, Before: filters.Before, Limit: 100})
	if err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusServiceUnavailable)
		return
	}
	records := logRecordViews(page.Records)
	content := logsContent{
		Filters: filters, Records: records, Degraded: page.Degraded,
		OlderURL: internalURL(logPageURL(filters, page.NextBefore, page.HasMore), dependencies.scenario), NewestURL: internalURL(logPageURL(filters, 0, filters.Before != 0), dependencies.scenario),
	}
	view := Page{Title: "Logs — Symphony", Route: "/logs", Heading: "Logs", Mode: handler.mode, Status: logStatus(len(records)), Scenario: dependencies.scenario, Content: content}
	if err := handler.renderHTML(w, "logs", view); err != nil {
		handler.respondHTMLRequestError(w, request, http.StatusInternalServerError)
	}
}

func parseLogFilters(values url.Values) logFilters {
	filters := logFilters{}
	if query, ok := normalizedFilterText(firstQueryValue(values, "query"), maximumLogQueryBytes); ok {
		filters.Query = query
	}
	level := strings.ToUpper(strings.TrimSpace(firstQueryValue(values, "level")))
	switch level {
	case "DEBUG", "INFO", "WARN", "ERROR":
		filters.Level = level
	}
	filters.Before = parseUnsignedCursor(firstQueryValue(values, "before"))
	return filters
}

func logStatus(count int) string {
	if count == 0 {
		return "No log entries are available."
	}
	if count == 1 {
		return "1 log entry is shown."
	}
	return strconv.Itoa(count) + " log entries are shown."
}

func logPageURL(filters logFilters, before uint64, available bool) string {
	if !available {
		return ""
	}
	values := make(url.Values)
	if filters.Query != "" {
		values.Set("query", filters.Query)
	}
	if filters.Level != "" {
		values.Set("level", filters.Level)
	}
	if before != 0 {
		values.Set("before", strconv.FormatUint(before, 10))
	}
	if encoded := values.Encode(); encoded != "" {
		return "/logs?" + encoded
	}
	return "/logs"
}

func logRecordViews(records []observability.LogRecord) []logRecordView {
	views := make([]logRecordView, 0, len(records))
	for _, record := range records {
		message, messageTruncated := cleanDisplay(record.Message, maximumLogMessage)
		fields := formatStructuredFields(record.Fields)
		fields, fieldsTruncated := truncateCleanText(fields, maximumLogFields)
		views = append(views, logRecordView{
			Sequence: record.Sequence, DateTime: record.Time.UTC().Format("2006-01-02T15:04:05Z07:00"), DisplayTime: record.Time.Local().Format("Jan 2, 2006 3:04:05 PM MST"),
			Level: cleanDisplayValue(record.Level, maximumShortTextBytes), Message: message, Fields: fields, MessageTruncated: messageTruncated, FieldsTruncated: fieldsTruncated,
		})
	}
	return views
}

func issueLogRecordViews(identifier string, records []observability.LogRecord) []logRecordView {
	filtered := make([]observability.LogRecord, 0, len(records))
	for _, record := range records {
		value, ok := record.Fields["issue_identifier"].(string)
		if !ok || cleanMachine(value, maximumIdentifierBytes) != identifier {
			continue
		}
		filtered = append(filtered, record)
	}
	return logRecordViews(filtered)
}

func truncateCleanText(value string, maximum int) (string, bool) {
	value = strings.ToValidUTF8(value, "�")
	if len(value) <= maximum {
		return value, false
	}
	cut := maximum
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…", true
}

type safeFieldFormatter struct {
	stack map[fieldVisit]struct{}
	items int
}

type fieldVisit struct {
	kind    reflect.Kind
	typeOf  reflect.Type
	pointer uintptr
}

func formatStructuredFields(fields map[string]any) string {
	formatter := safeFieldFormatter{stack: make(map[fieldVisit]struct{})}
	var output strings.Builder
	formatter.writeMap(&output, fields, 0)
	return output.String()
}

func (formatter *safeFieldFormatter) writeMap(output *strings.Builder, values map[string]any, depth int) {
	if values == nil {
		output.WriteString("{}")
		return
	}
	if depth > maximumLogFieldDepth || formatter.items+len(values) > maximumLogFieldItems || !formatter.enter(reflect.ValueOf(values)) {
		output.WriteString(`{"status":"structured_fields_unavailable"}`)
		return
	}
	defer formatter.leave(reflect.ValueOf(values))
	formatter.items += len(values)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	output.WriteByte('{')
	for index, key := range keys {
		if index > 0 {
			output.WriteByte(',')
		}
		writeJSONString(output, cleanDisplayValue(key, maximumShortTextBytes))
		output.WriteByte(':')
		formatter.writeValue(output, values[key], depth+1)
	}
	output.WriteByte('}')
}

func (formatter *safeFieldFormatter) writeSlice(output *strings.Builder, values []any, depth int) {
	if depth > maximumLogFieldDepth || formatter.items+len(values) > maximumLogFieldItems || !formatter.enter(reflect.ValueOf(values)) {
		output.WriteString(`{"status":"structured_fields_unavailable"}`)
		return
	}
	defer formatter.leave(reflect.ValueOf(values))
	formatter.items += len(values)
	output.WriteByte('[')
	for index, value := range values {
		if index > 0 {
			output.WriteByte(',')
		}
		formatter.writeValue(output, value, depth+1)
	}
	output.WriteByte(']')
}

func (formatter *safeFieldFormatter) writeValue(output *strings.Builder, value any, depth int) {
	if depth > maximumLogFieldDepth {
		output.WriteString(`{"status":"structured_fields_unavailable"}`)
		return
	}
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		output.WriteString(strconv.FormatBool(typed))
	case string:
		writeJSONString(output, cleanDisplayValue(typed, maximumLogFields))
	case []byte:
		writeJSONString(output, cleanDisplayValue(string(typed), maximumLogFields))
	case json.Number:
		if _, err := strconv.ParseFloat(string(typed), 64); err != nil {
			output.WriteString("null")
		} else {
			output.WriteString(string(typed))
		}
	case float32:
		formatter.writeFloat(output, float64(typed), 32)
	case float64:
		formatter.writeFloat(output, typed, 64)
	case int:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int8:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int16:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int32:
		output.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		output.WriteString(strconv.FormatInt(typed, 10))
	case uint:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint8:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint16:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint32:
		output.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		output.WriteString(strconv.FormatUint(typed, 10))
	case map[string]any:
		formatter.writeMap(output, typed, depth)
	case []any:
		formatter.writeSlice(output, typed, depth)
	case []string:
		values := make([]any, len(typed))
		for index, item := range typed {
			values[index] = item
		}
		formatter.writeSlice(output, values, depth)
	default:
		output.WriteString(`{"status":"structured_value_unavailable"}`)
	}
}

func (formatter *safeFieldFormatter) writeFloat(output *strings.Builder, value float64, bits int) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		output.WriteString("null")
		return
	}
	output.WriteString(strconv.FormatFloat(value, 'g', -1, bits))
}

func writeJSONString(output *strings.Builder, value string) {
	encoded, _ := json.Marshal(value)
	output.Write(encoded)
}

func (formatter *safeFieldFormatter) enter(value reflect.Value) bool {
	if !value.IsValid() || (value.Kind() != reflect.Map && value.Kind() != reflect.Slice) || value.IsNil() {
		return true
	}
	visit := fieldVisit{kind: value.Kind(), typeOf: value.Type(), pointer: value.Pointer()}
	if visit.pointer == 0 {
		return true
	}
	if _, found := formatter.stack[visit]; found {
		return false
	}
	formatter.stack[visit] = struct{}{}
	return true
}

func (formatter *safeFieldFormatter) leave(value reflect.Value) {
	if !value.IsValid() || (value.Kind() != reflect.Map && value.Kind() != reflect.Slice) || value.IsNil() {
		return
	}
	delete(formatter.stack, fieldVisit{kind: value.Kind(), typeOf: value.Type(), pointer: value.Pointer()})
}
