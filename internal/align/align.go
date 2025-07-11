package align

import (
	"errors"
	"fmt"
)

type Align int

const (
	Left Align = iota
	Right
)

func AlignColumns(rows [][]string, alignments []Align) ([][]string, error) {
	widths, err := columnWidths(rows)
	if err != nil {
		return nil, err
	}
	alignedRows := make([][]string, len(rows))
	for i, row := range rows {
		if i == 0 && len(row) != len(alignments) {
			return nil, errors.New("alignments count must be match to column count in rows")
		}
		alignedRows[i] = make([]string, len(row))
		for j, col := range row {
			var format string
			switch alignments[j] {
			case Left:
				format = "%-*s"
			case Right:
				format = "%*s"
			}
			alignedRows[i][j] = fmt.Sprintf(format, widths[j], col)
		}
	}
	return alignedRows, nil
}

func columnWidths(rows [][]string) ([]int, error) {
	if len(rows) == 0 {
		return nil, errors.New("no rows")
	}

	var widths []int
	for i, row := range rows {
		if i == 0 {
			widths = make([]int, len(row))
		} else if len(row) != len(widths) {
			return nil, errors.New("all column count must be same in rows")
		}

		for j, col := range row {
			widths[j] = max(widths[j], len(col))
		}
	}
	return widths, nil
}
