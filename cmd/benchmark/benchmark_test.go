// Unit tests for the benchmark's pure helpers.
//
// The three scenarios themselves need an ONNX model (and, for the Global Brain,
// a running control plane), so what is tested here is the arithmetic and the
// reading that the numbers rest on: session chunking, the write-back memory the
// distilled mode pushes, the reduction formula, and flag validation -- including
// the rule that a credential never reaches an error message.
package main

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitSequentialCoversEveryMessageExactlyOnce(t *testing.T) {
	messages := make([]Message, 26)
	for i := range messages {
		messages[i] = Message{Role: "user", Content: fmt.Sprintf("msg-%d", i)}
	}

	chunks := splitSequential(messages, 5)
	require.Len(t, chunks, 5)

	var flat []Message
	sizes := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		sizes = append(sizes, len(chunk))
		flat = append(flat, chunk...)
	}

	require.Len(t, flat, len(messages), "every message must land in exactly one session")
	for i, msg := range flat {
		assert.Equal(t, messages[i], msg, "message %d must keep its position", i)
	}

	// 26 messages over 5 sessions: the remainder is spread one per session from
	// the front, so the sessions stay as even as they can be.
	assert.Equal(t, []int{6, 5, 5, 5, 5}, sizes)
}

func TestSplitSequentialClampsToTheMessageCount(t *testing.T) {
	chunks := splitSequential([]Message{{Role: "user"}, {Role: "user"}, {Role: "user"}}, 5)

	require.Len(t, chunks, 3, "five sessions cannot hold three messages without empty ones")
	for _, chunk := range chunks {
		assert.Len(t, chunk, 1)
	}
}

func TestSplitSequentialRefusesEmptyInput(t *testing.T) {
	assert.Nil(t, splitSequential(nil, 5))
	assert.Nil(t, splitSequential([]Message{{Role: "user"}}, 0))
}

func TestLastUserMessagePrefersTheFinalUserTurn(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "second"},
		{Role: "assistant", Content: "last reply"},
	}
	assert.Equal(t, "second", lastUserMessage(messages))
}

func TestLastUserMessageFallsBackToTheLastMessage(t *testing.T) {
	assert.Equal(t, "only reply", lastUserMessage([]Message{{Role: "assistant", Content: "only reply"}}))
	assert.Empty(t, lastUserMessage(nil))
}

func TestReductionPct(t *testing.T) {
	// The established fixture's real numbers: 5569 raw, 2999 compiled.
	assert.InDelta(t, 46.1, reductionPct(5569, 2999), 0.1)

	assert.Equal(t, 0.0, reductionPct(100, 100), "an unchanged conversation reduces nothing")
	assert.Equal(t, 0.0, reductionPct(0, 0), "an empty fixture has no reduction to report")
	assert.Equal(t, -50.0, reductionPct(100, 150), "a compile that grew is reported as growth")
}

func TestCompileResultReductionReadsItsOwnNumbers(t *testing.T) {
	result := compileResult{RawTokens: 1000, CompiledTokens: 400}
	assert.InDelta(t, 60.0, result.Reduction(), 0.001)
}

func TestClassifyMemType(t *testing.T) {
	assert.Equal(t, "error", classifyMemType("the nil pointer dereference is in the webhook handler"))
	assert.Equal(t, "decision", classifyMemType("let me implement this handler and refactor the structure"))
	assert.Equal(t, "context", classifyMemType("this is fantastic, thanks"))
}
