package main

import (
	"bytes"
	"fmt"
	"image"
)

// validateImageBytes applies the common encoded-size and decoded-pixel bounds
// before any caller fully decodes image data.
func validateImageBytes(data []byte) (image.Config, string, error) {
	if len(data) == 0 || len(data) > maxLocalImageBytes {
		return image.Config{}, "", fmt.Errorf("image size is outside the supported range")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return image.Config{}, "", fmt.Errorf("unsupported image format or invalid image data: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 ||
		int64(config.Width)*int64(config.Height) > maxLocalImagePixels {
		return image.Config{}, "", fmt.Errorf("image dimensions are outside the supported range")
	}
	return config, format, nil
}
