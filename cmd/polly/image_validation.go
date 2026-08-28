package main

import (
	"image"

	"github.com/alexschlessinger/pollytool/images"
)

// validateImageBytes applies the common encoded-size and decoded-pixel bounds
// before any caller fully decodes image data.
func validateImageBytes(data []byte) (image.Config, string, error) {
	return images.Validate(data)
}
