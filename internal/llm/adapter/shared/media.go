package shared

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/dillonzq/apix/internal/errclass"
)

// MediaPart holds input media independently of the protocol envelope.
// File IDs are preserved only between OpenAI protocols; they are provider scoped.
type MediaPart struct {
	Kind       string
	FileID     string
	Data       string
	Filename   string
	URL        string
	Format     string
	Processing string
}

func decodeMediaPart(raw json.RawMessage, typ string) (*MediaPart, *errclass.Error) {
	var p struct {
		File *struct {
			FileID   string `json:"file_id"`
			Data     string `json:"file_data"`
			Filename string `json:"filename"`
			URL      string `json:"file_url"`
		} `json:"file"`
		FileID   string `json:"file_id"`
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
		FileURL  string `json:"file_url"`
		Audio    *struct {
			Data   string `json:"data"`
			Format string `json:"format"`
		} `json:"input_audio"`
		Data       string          `json:"data"`
		Format     string          `json:"format"`
		Video      json.RawMessage `json:"video_url"`
		Processing string          `json:"processing"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil, errclass.Translation("malformed media content part")
	}
	m := &MediaPart{}
	switch typ {
	case "file", "input_file":
		m.Kind, m.FileID, m.Data, m.Filename, m.URL = "file", p.FileID, p.FileData, p.Filename, p.FileURL
		if typ == "file" {
			if p.File == nil {
				return nil, errclass.Translation("file content part missing file")
			}
			m.FileID, m.Data, m.Filename, m.URL = p.File.FileID, p.File.Data, p.File.Filename, p.File.URL
		}
		if m.FileID == "" && m.Data == "" && m.URL == "" {
			return nil, errclass.Translation("file content part missing file_id, file_data or file_url")
		}
	case "input_audio":
		m.Kind, m.Data, m.Format = "audio", p.Data, p.Format
		if p.Audio != nil {
			if p.Audio.Data != "" {
				m.Data = p.Audio.Data
			}
			if p.Audio.Format != "" {
				m.Format = p.Audio.Format
			}
		}
		if m.Data == "" {
			return nil, errclass.Translation("audio content part missing data")
		}
	case "video_url", "input_video":
		m.Kind = "video"
		if json.Unmarshal(p.Video, &m.URL) != nil {
			var video struct {
				URL        string `json:"url"`
				Processing string `json:"processing"`
			}
			if json.Unmarshal(p.Video, &video) != nil {
				return nil, errclass.Translation("malformed video_url")
			}
			m.URL, m.Processing = video.URL, video.Processing
		}
		if p.Processing != "" {
			m.Processing = p.Processing
		}
		if m.URL == "" {
			return nil, errclass.Translation("video content part missing url")
		}
	}
	return m, nil
}

// OpenAI renders media in Chat Completions or Responses vocabulary.
func (m *MediaPart) OpenAI(responses bool) (map[string]any, *errclass.Error) {
	switch m.Kind {
	case "file":
		file := map[string]any{}
		for k, v := range map[string]string{"file_id": m.FileID, "file_data": m.Data, "filename": m.Filename, "file_url": m.URL} {
			if v != "" {
				file[k] = v
			}
		}
		if responses {
			file["type"] = "input_file"
			return file, nil
		}
		if m.URL != "" {
			return nil, UnsupportedPartType("file_url", "/v1/chat/completions")
		}
		return map[string]any{"type": "file", "file": file}, nil
	case "audio":
		audio := map[string]any{"data": m.Data}
		if m.Format != "" {
			audio["format"] = m.Format
		}
		return map[string]any{"type": "input_audio", "input_audio": audio}, nil
	case "video":
		video := map[string]any{"url": m.URL}
		if m.Processing != "" {
			video["processing"] = m.Processing
		}
		// Match CLIProxyAPI's extension shape and retain processing controls.
		if responses {
			return map[string]any{"type": "input_video", "video_url": video}, nil
		}
		return map[string]any{"type": "video_url", "video_url": video}, nil
	}
	return nil, UnsupportedPartType(m.Kind, "OpenAI")
}

func (m *MediaPart) Claude() (map[string]any, *errclass.Error) {
	if m.Kind != "file" {
		return nil, UnsupportedPartType(m.Kind, "/v1/messages")
	}
	if m.FileID != "" {
		return nil, UnsupportedPartType("file_id", "/v1/messages")
	}
	if m.Data != "" && m.URL != "" {
		return nil, UnsupportedPartType("file_data with file_url", "/v1/messages")
	}
	var source map[string]any
	if m.Data != "" {
		mime, data := "application/octet-stream", m.Data
		if strings.HasPrefix(data, "data:") {
			parts := strings.SplitN(strings.TrimPrefix(data, "data:"), ";base64,", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, errclass.Translation("malformed file data URL")
			}
			mime, data = parts[0], parts[1]
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, errclass.Translation("malformed base64 file data")
		}
		switch mime {
		case "application/pdf":
			source = map[string]any{"type": "base64", "media_type": "application/pdf", "data": data}
		case "text/plain":
			if !utf8.Valid(decoded) {
				return nil, errclass.Translation("text file data must be valid UTF-8")
			}
			source = map[string]any{"type": "text", "media_type": "text/plain", "data": string(decoded)}
		default:
			return nil, UnsupportedPartType("file media_type", "/v1/messages")
		}
	} else {
		source = map[string]any{"type": "url", "url": m.URL}
	}
	block := map[string]any{"type": "document", "source": source}
	if m.Filename != "" {
		block["title"] = m.Filename
	}
	return block, nil
}

func decodeClaudeDocument(raw json.RawMessage) (*MediaPart, *errclass.Error) {
	var block struct {
		Filename string `json:"filename"`
		Title    string `json:"title"`
		Source   *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
	}
	if json.Unmarshal(raw, &block) != nil || block.Source == nil {
		return nil, errclass.Translation("document block missing or malformed source")
	}
	m := &MediaPart{Kind: "file", Filename: block.Filename}
	if m.Filename == "" {
		m.Filename = block.Title
	}
	src := block.Source
	switch src.Type {
	case "base64":
		if src.Data == "" {
			return nil, errclass.Translation("document source missing data")
		}
		if _, err := base64.StdEncoding.DecodeString(src.Data); err != nil {
			return nil, errclass.Translation("malformed base64 document data")
		}
		mime := src.MediaType
		if mime == "" {
			mime = "application/octet-stream"
		}
		m.Data = "data:" + mime + ";base64," + src.Data
	case "url":
		if src.URL == "" {
			return nil, errclass.Translation("document source missing url")
		}
		m.URL = src.URL
	case "text":
		if src.Data == "" {
			return nil, errclass.Translation("document source missing data")
		}
		m.Data = "data:text/plain;base64," + base64.StdEncoding.EncodeToString([]byte(src.Data))
	default:
		return nil, UnsupportedPartType("document source "+src.Type, "OpenAI")
	}
	return m, nil
}
