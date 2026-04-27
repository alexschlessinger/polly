package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alexschlessinger/pollytool/llm"
	"github.com/urfave/cli/v3"
)

func embedCommand() *cli.Command {
	return &cli.Command{
		Name:  "embed",
		Usage: "Generate embedding vectors for text input",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "model",
				Aliases: []string{"m"},
				Usage:   "Embedding model (provider/model format)",
				Value:   "openai/text-embedding-3-large",
				Sources: cli.EnvVars("POLLYTOOL_EMBED_MODEL"),
				Validator: func(model string) error {
					return validateEmbedModel(model)
				},
			},
			&cli.IntFlag{
				Name:  "dimensions",
				Usage: "Output vector dimensions (0 = model default)",
				Value: 0,
			},
			&cli.StringFlag{
				Name:    "baseurl",
				Usage:   "Base URL for API (for OpenAI-compatible endpoints)",
				Sources: cli.EnvVars("POLLYTOOL_BASEURL"),
			},
			&cli.DurationFlag{
				Name:  "timeout",
				Usage: "Request timeout",
				Value: 2 * time.Minute,
			},
			&cli.StringSliceFlag{
				Name:    "input",
				Aliases: []string{"i"},
				Usage:   "Text to embed (can be specified multiple times)",
			},
			&cli.StringSliceFlag{
				Name:    "file",
				Aliases: []string{"f"},
				Usage:   "File whose contents to embed as a single vector (can be specified multiple times)",
			},
			&cli.StringFlag{
				Name:    "task-type",
				Usage:   "Gemini task type (e.g. RETRIEVAL_DOCUMENT, RETRIEVAL_QUERY, CLASSIFICATION)",
				Sources: cli.EnvVars("POLLYTOOL_EMBED_TASKTYPE"),
			},
			&cli.BoolFlag{
				Name:  "raw",
				Usage: "Output raw vectors only (one JSON array per line)",
			},
		},
		Action: runEmbed,
	}
}

func runEmbed(ctx context.Context, cmd *cli.Command) error {
	input, err := collectEmbedInput(cmd)
	if err != nil {
		return err
	}
	if len(input) == 0 {
		return fmt.Errorf("no input provided. use -i, -f, positional args, or stdin")
	}

	resp, err := llm.Embed(ctx, &llm.EmbeddingRequest{
		Model:      cmd.String("model"),
		BaseURL:    cmd.String("baseurl"),
		Timeout:    cmd.Duration("timeout"),
		Input:      input,
		Dimensions: int(cmd.Int("dimensions")),
		TaskType:   cmd.String("task-type"),
	})
	if err != nil {
		return err
	}

	if cmd.Bool("raw") {
		return outputRaw(resp)
	}
	return outputJSON(resp)
}

func collectEmbedInput(cmd *cli.Command) ([]string, error) {
	var inputs []string
	inputs = append(inputs, cmd.StringSlice("input")...)

	for _, path := range cmd.StringSlice("file") {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		inputs = append(inputs, string(data))
	}

	inputs = append(inputs, cmd.Args().Slice()...)

	if len(inputs) > 0 {
		return inputs, nil
	}
	if hasStdinData() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				inputs = append(inputs, line)
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
	}
	return inputs, nil
}

type embedOutput struct {
	Model       string      `json:"model"`
	Embeddings  [][]float64 `json:"embeddings"`
	InputTokens int         `json:"input_tokens"`
}

func outputJSON(resp *llm.EmbeddingResponse) error {
	out := embedOutput{
		Model:       resp.Model,
		Embeddings:  resp.Embeddings,
		InputTokens: resp.InputTokens,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func outputRaw(resp *llm.EmbeddingResponse) error {
	enc := json.NewEncoder(os.Stdout)
	for _, vec := range resp.Embeddings {
		if err := enc.Encode(vec); err != nil {
			return err
		}
	}
	return nil
}
