package install

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// This file updates the [mcp_servers.zalki] table of a Codex config in place.
// It replaces the value text of owned keys and copies every other byte of the
// file unchanged, so comments, order and the user's own keys stay as they are.
// It supports exactly one explicit [mcp_servers.zalki] header. It never adds or
// deletes a key: today zalki owns command and args, and no release changed that.

var (
	errZalkiLayout = errors.New("zalki must be one explicit [mcp_servers.zalki] table; rewrite it that way or delete it, then run zalki upgrade again")
	errZalkiUnsafe = errors.New("cannot update the zalki table safely; delete that table, then run zalki upgrade again")
)

// zalkiPath is the path of the managed table.
var zalkiPath = []string{"mcp_servers", zalkiMCPServer}

// tomlSpan is a byte range [start, end) of a document.
type tomlSpan struct{ start, end int }

// tomlEdit replaces the bytes of span with text.
type tomlEdit struct {
	span tomlSpan
	text []byte
}

// tableScan walks the expressions of a document and records the value range of
// each direct key of the managed table.
type tableScan struct {
	data    []byte
	path    []string
	inside  bool
	headers int
	values  map[string]tomlSpan
}

// keyParts returns the decoded parts of the (possibly dotted) key of a node.
func keyParts(node *unstable.Node) []string {
	var parts []string
	for it := node.Key(); it.Next(); {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

// overlapsZalki reports whether a full key path is, or lies under, the managed table's path.
func overlapsZalki(full []string) bool {
	n := min(len(full), len(zalkiPath))
	return slices.Equal(full[:n], zalkiPath[:n])
}

func visitExpression(scan *tableScan, node *unstable.Node) error {
	if node.Kind == unstable.KeyValue {
		return visitKeyValue(scan, node)
	}
	if node.Kind != unstable.Table && node.Kind != unstable.ArrayTable {
		return nil
	}
	scan.path = keyParts(node)
	scan.inside = slices.Equal(scan.path, zalkiPath)
	if scan.inside && node.Kind == unstable.ArrayTable {
		return errZalkiLayout
	}
	if scan.inside {
		scan.headers++
	}
	return nil
}

func visitKeyValue(scan *tableScan, node *unstable.Node) error {
	parts := keyParts(node)
	if scan.inside {
		if len(parts) != 1 {
			return nil // a dotted key inside the table is the user's
		}
		span, err := valueSpan(scan.data, node)
		if err != nil {
			return err
		}
		scan.values[parts[0]] = span
		return nil
	}
	underZalki := len(scan.path) >= len(zalkiPath) && slices.Equal(scan.path[:len(zalkiPath)], zalkiPath)
	if !underZalki && overlapsZalki(slices.Concat(scan.path, parts)) {
		return errZalkiLayout
	}
	return nil
}

// skipBlanks returns the first index at or after i that is not a space or a tab.
func skipBlanks(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	return i
}

// valueSpan returns the byte range of the value of a key/value expression. A
// value node can have no range of its own (an array has none, an inline table
// covers only its first byte), so the range is taken from the key/value node:
// after the "=" that follows the last key part, up to the end of that node.
func valueSpan(data []byte, node *unstable.Node) (tomlSpan, error) {
	keyEnd := 0
	for it := node.Key(); it.Next(); {
		raw := it.Node().Raw
		keyEnd = int(raw.Offset + raw.Length)
	}
	end := int(node.Raw.Offset + node.Raw.Length)
	eq := skipBlanks(data, keyEnd)
	if end > len(data) || eq >= end || data[eq] != '=' {
		return tomlSpan{}, errZalkiUnsafe
	}
	start := skipBlanks(data, eq+1)
	if start >= end {
		return tomlSpan{}, errZalkiUnsafe
	}
	return tomlSpan{start: start, end: end}, nil
}

// locateZalkiTable finds the managed table. found is false when the document
// does not define it at all. Any definition other than exactly one explicit
// [mcp_servers.zalki] header is refused.
func locateZalkiTable(data []byte) (values map[string]tomlSpan, found bool, err error) {
	scan := tableScan{data: data, values: map[string]tomlSpan{}}
	var parser unstable.Parser
	parser.Reset(data)
	for parser.NextExpression() {
		if err := visitExpression(&scan, parser.Expression()); err != nil {
			return nil, false, err
		}
	}
	if parser.Error() != nil {
		return nil, false, errZalkiUnsafe
	}
	switch {
	case scan.headers == 0:
		return nil, false, nil
	case scan.headers > 1:
		return nil, false, errZalkiLayout
	}
	return scan.values, true, nil
}

// splice replaces each edit's range. Edits must be sorted and must not overlap.
func splice(data []byte, edits []tomlEdit) []byte {
	var out bytes.Buffer
	pos := 0
	for _, edit := range edits {
		out.Write(data[pos:edit.span.start])
		out.Write(edit.text)
		pos = edit.span.end
	}
	out.Write(data[pos:])
	return out.Bytes()
}

// outsideUnchanged checks that after is before with only the edited ranges replaced.
func outsideUnchanged(before, after []byte, edits []tomlEdit) bool {
	posBefore, posAfter := 0, 0
	for _, edit := range edits {
		gap := before[posBefore:edit.span.start]
		gapEnd := posAfter + len(gap)
		textEnd := gapEnd + len(edit.text)
		if textEnd > len(after) || !bytes.Equal(after[posAfter:gapEnd], gap) || !bytes.Equal(after[gapEnd:textEnd], edit.text) {
			return false
		}
		posAfter, posBefore = textEnd, edit.span.end
	}
	return bytes.Equal(after[posAfter:], before[posBefore:])
}

// zalkiEdits builds the value replacements for the owned keys in plan. The new
// value text is copied from the next declared fragment, so it looks like a fresh install.
func zalkiEdits(before []byte, plan ownedPlan, next []byte) ([]tomlEdit, error) {
	if len(plan.remove) > 0 {
		return nil, errZalkiUnsafe
	}
	table, found, err := locateZalkiTable(before)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errZalkiLayout
	}
	fresh, freshFound, err := locateZalkiTable(next)
	if err != nil || !freshFound {
		return nil, errZalkiUnsafe
	}
	edits := make([]tomlEdit, 0, len(plan.set))
	for _, key := range plan.set {
		span, inFile := table[key]
		wanted, inNext := fresh[key]
		if !inFile || !inNext {
			return nil, errZalkiUnsafe
		}
		edits = append(edits, tomlEdit{span: span, text: next[wanted.start:wanted.end]})
	}
	slices.SortFunc(edits, func(a, b tomlEdit) int { return cmp.Compare(a.span.start, b.span.start) })
	for i := 1; i < len(edits); i++ {
		if edits[i].span.start < edits[i-1].span.end {
			return nil, errZalkiUnsafe
		}
	}
	return edits, nil
}

// sameTOML reports whether two decoded tables encode to the same TOML. Unlike
// a JSON comparison it keeps integers, floats and dates apart.
func sameTOML(a, b map[string]any) bool {
	x, errX := toml.Marshal(a)
	y, errY := toml.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

// documentWithoutZalki encodes a document without the managed table, so two
// documents can be compared outside it.
func documentWithoutZalki(data []byte) ([]byte, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if servers, ok := doc["mcp_servers"].(map[string]any); ok {
		delete(servers, zalkiMCPServer)
		if len(servers) == 0 {
			delete(doc, "mcp_servers")
		}
	}
	return toml.Marshal(doc)
}

// verifyZalkiEdit is the safety net. It refuses unless the new text differs from
// the old one only in the edited ranges, parses, holds the expected managed
// table, equals the old document everywhere else, and is a fixed point.
func verifyZalkiEdit(before, after []byte, edits []tomlEdit, current, wanted map[string]any, plan ownedPlan) error {
	if !outsideUnchanged(before, after, edits) {
		return errZalkiUnsafe
	}
	got, err := mcpNamedTable(after, zalkiMCPServer)
	if err != nil || got == nil {
		return errZalkiUnsafe
	}
	expected := maps.Clone(current)
	for _, key := range plan.set {
		expected[key] = wanted[key]
	}
	if !sameTOML(got, expected) {
		return errZalkiUnsafe
	}
	oldRest, errOld := documentWithoutZalki(before)
	newRest, errNew := documentWithoutZalki(after)
	if errOld != nil || errNew != nil || !bytes.Equal(oldRest, newRest) {
		return errZalkiUnsafe
	}
	again, problem := planOwnedKeys(got, wanted, nil)
	if problem != nil || len(again.set) > 0 || len(again.remove) > 0 {
		return errZalkiUnsafe
	}
	return nil
}

// recordedTables decodes the managed table of each recorded fragment.
func recordedTables(fragments []string) ([]map[string]any, error) {
	tables := make([]map[string]any, 0, len(fragments))
	for _, fragment := range fragments {
		_, table, err := mcpFragmentTable(fragment)
		if err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, nil
}

// updateMCPFragment brings the managed zalki table in before to next and keeps
// the user's keys. owned holds the recorded fragments of an upgrade; an install
// passes none, so an existing table is accepted only when it already has every
// owned key with the next value. Errors name the file.
func updateMCPFragment(path string, before []byte, owned []string, next string) ([]byte, error) {
	name, wanted, err := mcpFragmentTable(next)
	if err != nil {
		return nil, err
	}
	versions, err := recordedTables(owned)
	if err != nil {
		return nil, err
	}
	current, err := mcpNamedTable(before, name)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return addMCPFragment(before, next)
	}
	plan, problem := planOwnedKeys(current, wanted, versions)
	if problem != nil {
		return nil, fmt.Errorf("%s: %w", path, ownedKeyError(problem, mcpTableWording.label, mcpTableWording.noun, len(versions) > 0))
	}
	if len(plan.set) == 0 && len(plan.remove) == 0 {
		return before, nil
	}
	edits, err := zalkiEdits(before, plan, []byte(next))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	after := splice(before, edits)
	if err := verifyZalkiEdit(before, after, edits, current, wanted, plan); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return after, nil
}

// tableUserKeys decides what uninstall does with a managed table that is not
// exactly a recorded fragment. When it is one recorded table plus keys the user
// added, uninstall removes it and returns those key names. Otherwise it refuses.
func tableUserKeys(current map[string]any, fragments []string) ([]string, error) {
	tables, err := recordedTables(fragments)
	if err != nil {
		return nil, editedKeyError(nil, nil, mcpTableWording)
	}
	if extra, only := onlyUserKeysDiffer(current, tables); only {
		return extra, nil
	}
	return nil, editedKeyError(current, tables, mcpTableWording)
}

var (
	errZalkiRemoveLayout = errors.New("zalki must be one explicit [mcp_servers.zalki] table; rewrite it that way or delete it, then run zalki uninstall again")
	errZalkiRemoveUnsafe = errors.New("cannot remove the zalki table safely; delete it by hand, then run zalki uninstall again")
)

// isZalkiPath reports whether a table path is the managed table or lies under it.
func isZalkiPath(path []string) bool {
	return len(path) >= len(zalkiPath) && slices.Equal(path[:len(zalkiPath)], zalkiPath)
}

// lineStart returns the index of the first byte of the line that holds data[i].
func lineStart(data []byte, i int) int { return bytes.LastIndexByte(data[:i], '\n') + 1 }

// lineEnd returns the index after the line break of the line that holds data[i],
// or len(data) for a last line with no break.
func lineEnd(data []byte, i int) int {
	if next := bytes.IndexByte(data[i:], '\n'); next >= 0 {
		return i + next + 1
	}
	return len(data)
}

// zalkiBlocks returns the line ranges of the managed table and of every table
// or array of tables under it. A range starts at the line of the header, plus
// one empty line directly before it (LF or CRLF; a line with spaces does not
// count). It ends after the line of the last key/value of the block, or after
// the header line when the block has none. A table header has no byte range of
// its own, so its line comes from its first key part. Comments after the last
// key and blank lines stay: they belong to what follows.
func zalkiBlocks(data []byte) ([]tomlSpan, error) {
	var parser unstable.Parser
	parser.Reset(data)
	var blocks []tomlSpan
	inside := false
	for parser.NextExpression() {
		node := parser.Expression()
		if node.Kind == unstable.KeyValue && inside {
			blocks[len(blocks)-1].end = lineEnd(data, int(node.Raw.Offset+node.Raw.Length))
		}
		if node.Kind == unstable.Table || node.Kind == unstable.ArrayTable {
			if inside = isZalkiPath(keyParts(node)); inside {
				blocks = append(blocks, headerBlock(data, node, blocks))
			}
		}
	}
	if parser.Error() != nil {
		return nil, errZalkiRemoveUnsafe
	}
	return blocks, nil
}

// headerBlock returns the range of a header line, with the empty line before it.
func headerBlock(data []byte, header *unstable.Node, previous []tomlSpan) tomlSpan {
	first, last := -1, 0
	for it := header.Key(); it.Next(); {
		raw := it.Node().Raw
		if first < 0 {
			first = int(raw.Offset)
		}
		last = int(raw.Offset + raw.Length)
	}
	start := lineStart(data, max(first, 0))
	if start > 0 {
		before := lineStart(data, start-1)
		if line := data[before:start]; string(line) == "\n" || string(line) == "\r\n" {
			start = before
		}
	}
	if len(previous) > 0 {
		start = max(start, previous[len(previous)-1].end)
	}
	return tomlSpan{start: start, end: lineEnd(data, last)}
}

// removeZalkiTable removes the managed table and every table under it. It
// supports the layout the editor supports: one explicit [mcp_servers.zalki]
// header. Anything it cannot prove safe is refused and nothing is written.
func removeZalkiTable(before []byte) ([]byte, error) {
	if _, _, err := locateZalkiTable(before); err != nil {
		if errors.Is(err, errZalkiLayout) {
			return nil, errZalkiRemoveLayout
		}
		return nil, errZalkiRemoveUnsafe
	}
	blocks, err := zalkiBlocks(before)
	if err != nil || len(blocks) == 0 {
		return nil, errZalkiRemoveUnsafe
	}
	edits := make([]tomlEdit, len(blocks))
	for i, block := range blocks {
		edits[i] = tomlEdit{span: block}
	}
	after := splice(before, edits)
	if err := verifyZalkiRemoval(before, after, edits); err != nil {
		return nil, err
	}
	return after, nil
}

// verifyZalkiRemoval is the safety net of a removal. It refuses unless the new
// text is the old text with only the removed ranges gone, parses, has no
// managed table, and equals the old document everywhere else.
func verifyZalkiRemoval(before, after []byte, edits []tomlEdit) error {
	if !outsideUnchanged(before, after, edits) {
		return errZalkiRemoveUnsafe
	}
	if table, err := mcpNamedTable(after, zalkiMCPServer); err != nil || table != nil {
		return errZalkiRemoveUnsafe
	}
	oldRest, errOld := documentWithoutZalki(before)
	newRest, errNew := documentWithoutZalki(after)
	if errOld != nil || errNew != nil || !bytes.Equal(oldRest, newRest) {
		return errZalkiRemoveUnsafe
	}
	return nil
}
