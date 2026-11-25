package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"yoyaku/internal/pcm"
	"yoyaku/internal/worker"
)

func checkAudio(ctx context.Context, root string, args []string, out io.Writer) error {
	if len(args) != 2 || args[1] != "ko" && args[1] != "ja" {
		return fmt.Errorf("Use check-audio PATH.wav ko|ja")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	wav, err := pcm.Normalize(data)
	if err != nil {
		return err
	}
	client, err := worker.New(ctx, root)
	if err != nil {
		return err
	}
	result, err := client.Transcribe(ctx, wav, args[1], true)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}
