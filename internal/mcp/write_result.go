// The result half of synapse_write_memory: the wire shape the tool answers with.
//
// Split out of write.go on the same seam that split search_result.go from
// search.go -- by line count rather than by taste, and on the line the two have in
// common: everything here is about what a result looks like, and the handler is the
// only thing that decides how one is produced. Unlike search_result.go there is no
// mapping function beside it, because there is no scored type to flatten: a write
// answers with its own four facts and nothing derived.
package mcp

// writeToolResult is the payload synapse_write_memory returns.
//
// It carries no content of any kind, sanitized or otherwise: the caller already
// holds what it sent, so repeating it would only put a memory into a model's context
// twice. Every field is marshaled unconditionally -- conflict_with_id is an empty
// string, never an absent key -- so a caller can read "no conflict" off the response
// instead of having to interpret a missing field.
type writeToolResult struct {
	// ID is the uuid the memory was stored under, which is what a later
	// supersession or conflict reference has to name.
	ID string `json:"id"`
	// ConflictDetected reports whether the memory contradicts one already stored.
	ConflictDetected bool `json:"conflict_detected"`
	// ConflictWithID is the memory it contradicts, empty when none.
	ConflictWithID string `json:"conflict_with_id"`
	// Sanitized reports whether the sanitization pipeline rewrote the content.
	// The stored memory is the rewritten one; this says it happened.
	Sanitized bool `json:"sanitized"`
}
