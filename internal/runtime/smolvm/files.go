package smolvm

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"sandbox-platform/internal/runtime"
)

const binaryType = "application/octet-stream"

func filePath(name, guestPath string) (string, error) {
	if !path.IsAbs(guestPath) || guestPath == "/" || path.Clean(guestPath) != guestPath || strings.ContainsRune(guestPath, 0) {
		return "", invalid("guest file path must be an absolute, canonical path")
	}
	segments := strings.Split(strings.TrimPrefix(guestPath, "/"), "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return machinePath(name, "/files/"+strings.Join(segments, "/"))
}

// UploadFile and DownloadFile must be called with the platform's lifecycle and
// activity protection. The native files API may implicitly start a stopped VM.
func (client *Client) UploadFile(ctx context.Context, name, guestPath string, source io.Reader) error {
	return client.UploadFileWithOptions(ctx, name, guestPath, source, runtime.FileOptions{Size: -1})
}

func (client *Client) UploadFileWithOptions(ctx context.Context, name, guestPath string, source io.Reader, options runtime.FileOptions) error {
	path, err := filePath(name, guestPath)
	if err != nil {
		return err
	}
	if source == nil || options.Size < -1 {
		return invalid("missing file source or invalid length")
	}
	query := make(url.Values)
	if options.Mode != nil {
		if *options.Mode > 0o7777 {
			return invalid("file mode must contain only permission bits")
		}
		query.Set("mode", fmt.Sprintf("%04o", *options.Mode))
	}
	if options.UID != nil {
		query.Set("uid", strconv.FormatUint(uint64(*options.UID), 10))
	}
	if options.GID != nil {
		query.Set("gid", strconv.FormatUint(uint64(*options.GID), 10))
	}
	response, err := client.request(ctx, http.MethodPut, path, query, binaryType, source, options.Size)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var result struct {
		Size *uint64 `json:"size"`
	}
	if err := decode(response.Body, &result, true); err != nil {
		return err
	}
	if result.Size == nil || (options.Size >= 0 && *result.Size != uint64(options.Size)) {
		return protocolError("runtime did not acknowledge the uploaded file length", true, nil)
	}
	return nil
}

func (client *Client) DownloadFile(ctx context.Context, name, guestPath string, dst io.Writer) error {
	path, err := filePath(name, guestPath)
	if err != nil {
		return err
	}
	_, err = client.stream(ctx, http.MethodGet, path, nil, dst, binaryType)
	return err
}

func (client *Client) stream(ctx context.Context, method, path string, query url.Values, dst io.Writer, expectedMediaType string) (int64, error) {
	if dst == nil {
		return 0, invalid("missing stream destination")
	}
	response, err := client.request(ctx, method, path, query, "", nil, -1)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != expectedMediaType {
		return 0, protocolError("runtime stream did not return the expected binary content", method != http.MethodGet, err)
	}
	n, err := io.Copy(dst, response.Body)
	if err != nil {
		return n, protocolError("copying runtime stream: "+err.Error(), method != http.MethodGet, err)
	}
	return n, nil
}
