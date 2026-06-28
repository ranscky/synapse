package embedder

import (
	"bufio"
	"os"
	"strings"
	"unicode"
)

// WordPieceTokenizer implements BERT-style WordPiece tokenization, matching
// the behavior of HuggingFace's BertTokenizer with do_lower_case=true. This
// is the tokenizer all-MiniLM-L6-v2 (and BERT-family models generally)
// expect as input - getting this wrong produces no error, just silently
// incorrect embeddings, so this implementation is verified against real
// reference output from Python's transformers library (see
// wordpiece_tokenizer_test.go) rather than trusted on inspection alone.
type WordPieceTokenizer struct {
	vocab      map[string]int64
	unkTokenID int64
	clsTokenID int64
	sepTokenID int64
	padTokenID int64
	maxWordLen int // words longer than this become [UNK] directly, matching
	// BertTokenizer's default max_input_chars_per_word=100
}

// NewWordPieceTokenizer loads a vocabulary file (one token per line, line
// number == token ID) and returns a ready-to-use tokenizer. Special token
// IDs are looked up by their standard names rather than hardcoded, so this
// works correctly even if a vocabulary ever reorders them, though in
// practice every standard BERT vocab.txt uses [PAD]=0, [UNK]=100, [CLS]=101,
// [SEP]=102.
func NewWordPieceTokenizer(vocabPath string) (*WordPieceTokenizer, error) {
	file, err := os.Open(vocabPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	vocab := make(map[string]int64)
	scanner := bufio.NewScanner(file)
	var lineNum int64
	for scanner.Scan() {
		token := scanner.Text()
		vocab[token] = lineNum
		lineNum++
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	t := &WordPieceTokenizer{
		vocab:      vocab,
		maxWordLen: 100,
	}

	// Look up special tokens by name rather than assuming fixed IDs.
	t.unkTokenID = vocab["[UNK]"]
	t.clsTokenID = vocab["[CLS]"]
	t.sepTokenID = vocab["[SEP]"]
	t.padTokenID = vocab["[PAD]"]

	return t, nil
}

// Encode tokenizes a single sentence and returns input_ids, attention_mask,
// and token_type_ids, all of length seqLen (padded with [PAD]/0 if the
// real token count is shorter, truncated if longer - truncation reserves
// room for [CLS] and [SEP] so they're never cut off).
//
// token_type_ids is all zeros here since this only supports single-sentence
// input (no sentence-pair tasks), which is all Synapse needs.
func (t *WordPieceTokenizer) Encode(text string, seqLen int) (inputIDs, attentionMask, tokenTypeIDs []int64) {
	wordPieceTokens := t.tokenize(text)

	// Reserve 2 slots for [CLS] and [SEP].
	maxContentLen := seqLen - 2
	if maxContentLen < 0 {
		maxContentLen = 0
	}
	if len(wordPieceTokens) > maxContentLen {
		wordPieceTokens = wordPieceTokens[:maxContentLen]
	}

	ids := make([]int64, 0, seqLen)
	ids = append(ids, t.clsTokenID)
	ids = append(ids, wordPieceTokens...)
	ids = append(ids, t.sepTokenID)

	realLen := len(ids)

	inputIDs = make([]int64, seqLen)
	attentionMask = make([]int64, seqLen)
	tokenTypeIDs = make([]int64, seqLen) // all zeros by default - correct for single-sentence input

	for i := 0; i < seqLen; i++ {
		if i < realLen {
			inputIDs[i] = ids[i]
			attentionMask[i] = 1
		} else {
			inputIDs[i] = t.padTokenID
			attentionMask[i] = 0
		}
	}

	return inputIDs, attentionMask, tokenTypeIDs
}

// tokenize converts raw text into a flat list of WordPiece token IDs
// (not yet including [CLS]/[SEP] - those are added by Encode).
func (t *WordPieceTokenizer) tokenize(text string) []int64 {
	var result []int64
	for _, word := range basicTokenize(text) {
		result = append(result, t.wordPieceSplit(word)...)
	}
	return result
}

// basicTokenize lowercases the input and splits it into words and
// standalone punctuation tokens, matching BertTokenizer's BasicTokenizer
// behavior: whitespace is a separator, and every punctuation/symbol
// character becomes its own single-character token rather than staying
// attached to adjacent letters/digits.
func basicTokenize(text string) []string {
	text = strings.ToLower(text)

	var tokens []string
	var current strings.Builder

	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}

	for _, r := range text {
		switch {
		case unicode.IsSpace(r):
			flush()
		case isPunctuationOrSymbol(r):
			flush()
			tokens = append(tokens, string(r))
		default:
			current.WriteRune(r)
		}
	}
	flush()

	return tokens
}

// isPunctuationOrSymbol matches BertTokenizer's _is_punctuation check:
// any ASCII punctuation/symbol character, plus Unicode punctuation and
// symbol categories. This deliberately treats characters like '-', ',',
// '.', etc. as their own tokens, matching the reference behavior observed
// (e.g. "COVID-19" splits into "covid", "-", "19").
func isPunctuationOrSymbol(r rune) bool {
	if unicode.IsPunct(r) || unicode.IsSymbol(r) {
		return true
	}
	// ASCII ranges BERT's tokenizer treats as punctuation even though Go's
	// unicode.IsPunct doesn't always categorize them that way (e.g. some
	// math/currency symbols).
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) || (r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return false
}

// wordPieceSplit applies greedy longest-match-first sub-word splitting to a
// single word (already lowercased, already isolated from punctuation).
// Continuation pieces (anything after the first piece) are prefixed with
// "##" before vocabulary lookup, matching standard WordPiece convention.
// If no valid split exists (some piece can't be matched at all), the
// entire word becomes a single [UNK] token - matching reference behavior
// for out-of-vocabulary words like "nil" -> "ni" + "##l", or "unbelievable"
// matching as a whole word when it IS in vocabulary.
func (t *WordPieceTokenizer) wordPieceSplit(word string) []int64 {
	if len(word) == 0 {
		return nil
	}
	if len([]rune(word)) > t.maxWordLen {
		return []int64{t.unkTokenID}
	}

	runes := []rune(word)
	var result []int64
	start := 0
	isFirstPiece := true

	for start < len(runes) {
		end := len(runes)
		var matchedID int64 = -1

		for end > start {
			substr := string(runes[start:end])
			if !isFirstPiece {
				substr = "##" + substr
			}
			if id, ok := t.vocab[substr]; ok {
				matchedID = id
				break
			}
			end--
		}

		if matchedID == -1 {
			// No valid piece found anywhere in the remainder - the whole
			// word is unknown, matching reference tokenizer behavior.
			return []int64{t.unkTokenID}
		}

		result = append(result, matchedID)
		start = end
		isFirstPiece = false
	}

	return result
}