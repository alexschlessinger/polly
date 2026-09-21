package images

import (
	"bytes"
	"fmt"
	"image"
	"io"
	"os"

	"github.com/alexschlessinger/pollytool/internal/safefile"
)

// openBounded opens a regular file of at most maxBytes bytes. path is trusted
// as spelled: symbolic links are followed, and a special file is refused
// without blocking on it. Readers still bound what they read, because the
// file can grow after this check.
func openBounded(path string, maxBytes int64) (*os.File, error) {
	file, err := safefile.OpenRegularFollow(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if info.Size() > maxBytes {
		_ = file.Close()
		return nil, errExceedsLimit(maxBytes)
	}
	return file, nil
}

func errExceedsLimit(maxBytes int64) error {
	return fmt.Errorf("file exceeds the %d MiB limit", maxBytes>>20)
}

// ReadBoundedFile reads a regular file of at most maxBytes bytes, following
// symbolic links and refusing special files without blocking on them.
func ReadBoundedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := openBounded(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errExceedsLimit(maxBytes)
	}
	return data, nil
}

// DecodeBoundedFile reads an image file within maxBytes, applies validate,
// and decodes it, reporting the detected format name.
func DecodeBoundedFile(path string, maxBytes int64) (image.Image, string, error) {
	data, err := ReadBoundedFile(path, maxBytes)
	if err != nil {
		return nil, "", err
	}
	if _, _, err := validate(data); err != nil {
		return nil, "", err
	}
	return image.Decode(bytes.NewReader(data))
}

// DecodeBoundedConfig reads the header of an image file within maxBytes and
// applies the source dimension bounds without decoding pixels.
func DecodeBoundedConfig(path string, maxBytes int64) (image.Config, string, error) {
	file, err := openBounded(path, maxBytes)
	if err != nil {
		return image.Config{}, "", err
	}
	defer file.Close()
	return decodeConfig(io.LimitReader(file, maxBytes+1))
}
