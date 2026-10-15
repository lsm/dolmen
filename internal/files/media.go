package files

import (
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/lsm/dolmen/internal/schema"
)

var extensionTypes = map[string]string{
	".aac":     "audio/aac",
	".avi":     "video/x-msvideo",
	".avif":    "image/avif",
	".bmp":     "image/bmp",
	".csv":     "text/csv",
	".doc":     "application/msword",
	".docx":    "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".epub":    "application/epub+zip",
	".flac":    "audio/flac",
	".gif":     "image/gif",
	".gz":      "application/gzip",
	".heic":    "image/heic",
	".htm":     "text/html",
	".html":    "text/html",
	".ico":     "image/vnd.microsoft.icon",
	".jpeg":    "image/jpeg",
	".jpg":     "image/jpeg",
	".js":      "text/javascript",
	".json":    "application/json",
	".jsonl":   "application/jsonl",
	".m4a":     "audio/mp4",
	".md":      "text/markdown",
	".mkv":     "video/x-matroska",
	".mov":     "video/quicktime",
	".mp3":     "audio/mpeg",
	".mp4":     "video/mp4",
	".mpeg":    "video/mpeg",
	".odp":     "application/vnd.oasis.opendocument.presentation",
	".ods":     "application/vnd.oasis.opendocument.spreadsheet",
	".odt":     "application/vnd.oasis.opendocument.text",
	".oga":     "audio/ogg",
	".ogg":     "audio/ogg",
	".ogv":     "video/ogg",
	".opus":    "audio/opus",
	".parquet": "application/vnd.apache.parquet",
	".pdf":     "application/pdf",
	".png":     "image/png",
	".ppt":     "application/vnd.ms-powerpoint",
	".pptx":    "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".rtf":     "application/rtf",
	".svg":     "image/svg+xml",
	".tar":     "application/x-tar",
	".tif":     "image/tiff",
	".tiff":    "image/tiff",
	".tsv":     "text/tab-separated-values",
	".txt":     "text/plain",
	".wav":     "audio/wav",
	".weba":    "audio/webm",
	".webm":    "video/webm",
	".webp":    "image/webp",
	".xls":     "application/vnd.ms-excel",
	".xlsx":    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".xml":     "application/xml",
	".yaml":    "application/yaml",
	".yml":     "application/yaml",
	".zip":     "application/zip",
}

const octetStream = "application/octet-stream"

func BaseMediaType(v string) string {
	if v == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(v)
	if err != nil {
		return ""
	}
	return strings.ToLower(mt)
}

func InferMediaType(declared, filename string, head []byte) string {
	if mt := BaseMediaType(declared); mt != "" && mt != octetStream && schema.ValidateMediaType("", mt) == nil {
		return mt
	}
	if t, ok := extensionTypes[strings.ToLower(path.Ext(filename))]; ok {
		return t
	}
	if len(head) > 0 {
		if mt := BaseMediaType(http.DetectContentType(head)); mt != "" {
			return mt
		}
	}
	return octetStream
}
