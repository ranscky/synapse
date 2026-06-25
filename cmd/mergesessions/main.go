
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	Messages []Message `json:"messages"`
}

func main() {
	if len(os.Args) < 4 {
		log.Fatal("Usage: merge_sessions <file1.json> <file2.json> <output.json>")
	}

	s1, err := loadSession(os.Args[1])
	if err != nil {
		log.Fatalf("failed to load %s: %v", os.Args[1], err)
	}
	s2, err := loadSession(os.Args[2])
	if err != nil {
		log.Fatalf("failed to load %s: %v", os.Args[2], err)
	}

	var merged Session

	// Keep only the first file's system prompt (if present)
	for _, m := range s1.Messages {
		if m.Role == "system" {
			merged.Messages = append(merged.Messages, m)
			break
		}
	}

	// Append all non-system messages from file 1
	for _, m := range s1.Messages {
		if m.Role != "system" {
			merged.Messages = append(merged.Messages, m)
		}
	}

	// Append all non-system messages from file 2 (its system prompt, if any, is dropped)
	for _, m := range s2.Messages {
		if m.Role != "system" {
			merged.Messages = append(merged.Messages, m)
		}
	}

	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		log.Fatalf("failed to marshal merged session: %v", err)
	}

	if err := os.WriteFile(os.Args[3], out, 0644); err != nil {
		log.Fatalf("failed to write output: %v", err)
	}

	fmt.Printf("Merged %d + %d = %d messages → %s\n",
		len(s1.Messages), len(s2.Messages), len(merged.Messages), os.Args[3])
}

func loadSession(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}