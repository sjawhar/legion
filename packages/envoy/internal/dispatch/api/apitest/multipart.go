// Package apitest holds what tests in other packages share to call the API's routes.
package apitest

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/textproto"
)

// MultipartUpload is the body of a multipart upload of content as the file filename beside fields,
// as POST /api/v1/issues/{key}/artifacts and POST /api/v1/projects/{key}/artifacts take one, and
// the Content-Type to send it with. The file part carries contentType, or no Content-Type of its
// own when it is empty.
func MultipartUpload(fields map[string]string, filename, contentType string, content []byte) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			return nil, "", fmt.Errorf("write multipart field %s: %w", key, err)
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("create multipart file part: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, "", fmt.Errorf("write multipart file: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish multipart body: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}
