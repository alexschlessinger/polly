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
			&cli.BoolFlag{
				Name:  "raw",
				Usage: "Output raw vectors only (one JSON array per line)",
			},
		},
		Action: runEmbed,
	}
}

func runEmbed(ctx context.Context, cmd *cli.Command) error {
	input := collectEmbedInput(cmd)
	if len(input) == 0 {
		return fmt.Errorf("no input provided. use -i flags, positional args, or stdin")
	}

	resp, err := llm.Embed(ctx, &llm.EmbeddingRequest{
		Model:      cmd.String("model"),
		BaseURL:    cmd.String("baseurl"),
		Timeout:    cmd.Duration("timeout"),
		Input:      input,
		Dimensions: int(cmd.Int("dimensions")),
	})
	if err != nil {
		return err
	}

	if cmd.Bool("raw") {
		return outputRaw(resp)
	}
	return outputJSON(resp)
}

func collectEmbedInput(cmd *cli.Command) []string {
	// priority: -i flags, then positional args, then stdin
	if inputs := cmd.StringSlice("input"); len(inputs) > 0 {
		return inputs
	}
	if args := cmd.Args().Slice(); len(args) > 0 {
		return args
	}
	if hasStdinData() {
		scanner := bufio.NewScanner(os.Stdin)
		var lines []string
		for scanner.Scan() {
			if line := strings.TrimSpace(scanner.Text()); line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
	return nil
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
