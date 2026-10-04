package server

import "strings"

// diffRow is one row of the side-by-side diff on the edit page.
type diffRow struct {
	Kind            string // "same", "del", "add", "change"
	LeftNo, RightNo int    // 0 means no line on that side
	Left, Right     string
}

// maxDiffCells caps the LCS table. Compose files are small; past this the
// page shows the new text without a diff instead of burning memory.
const maxDiffCells = 4_000_000

// sideBySide diffs two texts line by line using a longest common
// subsequence table. ok is false when the inputs are too large to diff.
func sideBySide(oldText, newText string) (rows []diffRow, changed bool, ok bool) {
	a, b := splitLines(oldText), splitLines(newText)
	n, m := len(a), len(b)
	if (n+1)*(m+1) > maxDiffCells {
		return nil, oldText != newText, false
	}
	// lcs[i][j] = length of the LCS of a[i:] and b[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var dels, adds []diffRow
	flush := func() {
		// Pair deletions with additions so a changed line sits on one row.
		k := 0
		for ; k < len(dels) && k < len(adds); k++ {
			rows = append(rows, diffRow{Kind: "change", LeftNo: dels[k].LeftNo, Left: dels[k].Left, RightNo: adds[k].RightNo, Right: adds[k].Right})
		}
		rows = append(rows, dels[k:]...)
		rows = append(rows, adds[k:]...)
		dels, adds = dels[:0], adds[:0]
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			flush()
			rows = append(rows, diffRow{Kind: "same", LeftNo: i + 1, Left: a[i], RightNo: j + 1, Right: b[j]})
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			adds = append(adds, diffRow{Kind: "add", RightNo: j + 1, Right: b[j]})
			changed = true
			j++
		default:
			dels = append(dels, diffRow{Kind: "del", LeftNo: i + 1, Left: a[i]})
			changed = true
			i++
		}
	}
	flush()
	if oldText != newText {
		changed = true // e.g. only a trailing newline differs
	}
	return rows, changed, true
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
