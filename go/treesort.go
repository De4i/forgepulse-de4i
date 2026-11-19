package main

// sortTreeEntries applies Git's tree ordering: entries sort by name, except a
// subtree's name is compared with an implicit trailing slash.
func sortTreeEntries(entries []TreeEntry) {
	if len(entries) < 2 {
		return
	}
	// Insertion sort: trees are tiny, and this keeps the call allocation-free.
	for i := 1; i < len(entries); i++ {
		cur := entries[i]
		j := i - 1
		for j >= 0 && treeLess(cur, entries[j]) {
			entries[j+1] = entries[j]
			j--
		}
		entries[j+1] = cur
	}
}

// treeLess compares two entries the way Git orders them.
func treeLess(a, b TreeEntry) bool {
	return treeKey(a) < treeKey(b)
}

// treeKey renders an entry's comparison key, using a trailing slash for trees.
func treeKey(e TreeEntry) string {
	if isDirectoryMode(e.Mode) {
		return e.Name + "/"
	}
	return e.Name
}

// sortIssuesByNumber orders issues by number. Descending is the default the
// contract asks for, and equal numbers (impossible here) would keep order.
func sortIssuesByNumber(rows []*Issue, ascending bool) {
	if len(rows) < 2 {
		return
	}
	for i := 1; i < len(rows); i++ {
		cur := rows[i]
		j := i - 1
		for j >= 0 {
			swap := rows[j].Number > cur.Number
			if !ascending {
				swap = rows[j].Number < cur.Number
			}
			if !swap {
				break
			}
			rows[j+1] = rows[j]
			j--
		}
		rows[j+1] = cur
	}
}
