package app

import "context"

type File struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
}

type Torrent struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Size         int64   `json:"size"`
	Downloaded   int64   `json:"downloaded"`
	Uploaded     int64   `json:"uploaded"`
	Progress     float64 `json:"progress"`
	DownloadRate float64 `json:"download_rate"`
	UploadRate   float64 `json:"upload_rate"`
	Peers        int     `json:"peers"`
	ETASeconds   int64   `json:"eta_seconds"`
	Ratio        float64 `json:"ratio"`
	Files        []File  `json:"files"`
}

type Service interface {
	AddMagnet(context.Context, string) (Torrent, error)
	AddTorrentFile(context.Context, string) (Torrent, error)
	Torrents(context.Context) ([]Torrent, error)
	Torrent(context.Context, string) (Torrent, error)
	Pause(context.Context, string) error
	Resume(context.Context, string) error
	Remove(context.Context, string, bool) error
}
