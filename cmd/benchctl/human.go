package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// renderHuman prints any JSON-serialisable value as aligned text:
// objects as key/value lines, arrays of objects as tables.
func renderHuman(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var g any
	if err := json.Unmarshal(b, &g); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	renderValue(tw, g, "")
	return tw.Flush()
}

func renderValue(w io.Writer, v any, indent string) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range sortedKeys(t) {
			val := t[k]
			if isScalar(val) || isScalarList(val) {
				_, _ = fmt.Fprintf(w, "%s%s:\t%s\n", indent, k, scalar(val))
				continue
			}
			_, _ = fmt.Fprintf(w, "%s%s:\n", indent, k)
			renderValue(w, val, indent+"  ")
		}
	case []any:
		if rows, cols := tableShape(t); rows != nil {
			_, _ = fmt.Fprintf(w, "%s%s\n", indent, strings.ToUpper(strings.Join(cols, "\t")))
			for _, r := range rows {
				cells := make([]string, len(cols))
				for i, c := range cols {
					cells[i] = scalar(r[c])
				}
				_, _ = fmt.Fprintf(w, "%s%s\n", indent, strings.Join(cells, "\t"))
			}
			return
		}
		for i, item := range t {
			if isScalar(item) {
				_, _ = fmt.Fprintf(w, "%s- %s\n", indent, scalar(item))
				continue
			}
			_, _ = fmt.Fprintf(w, "%s[%d]\n", indent, i)
			renderValue(w, item, indent+"  ")
		}
	default:
		_, _ = fmt.Fprintf(w, "%s%s\n", indent, scalar(v))
	}
}

// tableShape returns rows/columns if every element is an object of scalars.
func tableShape(items []any) ([]map[string]any, []string) {
	if len(items) == 0 {
		return nil, nil
	}
	var rows []map[string]any
	colSet := map[string]bool{}
	var cols []string
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			return nil, nil
		}
		for _, v := range m {
			if !isScalar(v) && !isScalarList(v) {
				return nil, nil // nested rows read better as blocks
			}
		}
		for _, k := range sortedKeys(m) {
			if !colSet[k] {
				colSet[k] = true
				cols = append(cols, k)
			}
		}
		rows = append(rows, m)
	}
	return rows, cols
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isScalar(v any) bool {
	switch v.(type) {
	case nil, string, float64, bool:
		return true
	}
	return false
}

func isScalarList(v any) bool {
	l, ok := v.([]any)
	if !ok || len(l) > 16 {
		return false
	}
	for _, x := range l {
		if !isScalar(x) {
			return false
		}
	}
	return true
}

func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return "-"
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'g', 8, 64)
	case bool:
		return strconv.FormatBool(t)
	case []any:
		if isScalarList(t) {
			parts := make([]string, len(t))
			for i, x := range t {
				parts[i] = scalar(x)
			}
			return strings.Join(parts, ", ")
		}
	}
	b, _ := json.Marshal(v)
	return string(b)
}
