package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/pkoukk/tiktoken-go"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	Messages []Message `json:"messages"`
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("Usage: count_tokens <session_file.json>")
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatalf("failed to read file: %v", err)
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		log.Fatalf("failed to parse JSON: %v", err)
	}

	tke, err := tiktoken.EncodingForModel("gpt-3.5-turbo")
	if err != nil {
		tke, err = tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			log.Fatalf("failed to init tokenizer: %v", err)
		}
	}

	total := 0
	systemTotal := 0
	for i, msg := range session.Messages {
		tokens := tke.Encode(msg.Content, nil, nil)
		count := len(tokens)
		total += count
		if msg.Role == "system" {
			systemTotal += count
		}
		fmt.Printf("  [%2d] %-10s %4d tokens — %.50s\n", i, msg.Role, count, msg.Content)
	}

	fmt.Println("---")
	fmt.Printf("Total messages: %d\n", len(session.Messages))
	fmt.Printf("Total tokens (all messages): %d\n", total)
	fmt.Printf("System prompt tokens: %d\n", systemTotal)
	fmt.Printf("Scorable (non-system) tokens: %d\n", total-systemTotal)
}