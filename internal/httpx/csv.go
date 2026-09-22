package httpx

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strings"
)

// CSVSafe neutralises spreadsheet formula injection: a cell that starts with
// = + - @ (or a tab/CR) would be executed by Excel, so it is prefixed with '.
func CSVSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// WriteCSV streams rows as a downloadable CSV file.
func WriteCSV(w http.ResponseWriter, filename string, header []string, rows [][]string) error {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return err
	}
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, c := range row {
			safe[i] = CSVSafe(c)
		}
		if err := cw.Write(safe); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}
