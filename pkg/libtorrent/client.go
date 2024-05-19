package libtorrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

type Client struct {
	// client / project name, will be the default directory name
	Name string
	// directory to download torrents to
	DataDir string
	// Seed or no
	Seed bool
	// Port to stream torrents on
	Port string
	// Port to stream torrents on
	TorrentPort int
	// Default torrent client options
	TorrentClient *torrent.Client
	// server
	srv *http.Server
	// torrents
	Torrents []*torrent.Torrent
	// Disable IPV6
	DisableIPV6 bool
}

// create a default client, must call Init afterwords
func NewClient(name string, port string) *Client {
	return &Client{
		Name: name,
		Port: port,
		Seed: false,
	}
}

// Initialize torrent configuration
func (c *Client) Init() error {
	cfg := torrent.NewDefaultClientConfig()
	s, err := c.getStorage()
	if err != nil {
		return err
	}

	cfg.DisableIPv6 = c.DisableIPV6

	// sanity check
	if c.TorrentPort == -1 {
		c.TorrentPort = 42069
	}

	cfg.ListenPort = c.TorrentPort
	c.DataDir = s
	cfg.DefaultStorage = storage.NewFileByInfoHash(c.DataDir)

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("error creating a new torrent client: %v", err)
	}

	c.StartServer()
	c.TorrentClient = client
	return nil
}

func (c *Client) DownloadTorrent(torrent string) error {
	t, err := c.AddTorrent(torrent)
	if err != nil {
		return err
	}
	t.DownloadAll()
	return nil
}

func (c *Client) ServeTorrents(ctx context.Context, torrents []*torrent.Torrent) {
	for _, t := range torrents {
		// it doesn't matter what episode included here, so it's just 0
		link := c.ServeTorrent(t, 0)
		fmt.Println(link)
	}
}

func getSortedFilesList(t *torrent.Torrent) []*torrent.File {
	// Almost zero copy cause pointers used
	files := make([]*torrent.File, len(t.Files()))
	copy(files, t.Files())

	slices.SortFunc(files, func(a, b *torrent.File) int {
		return strings.Compare(a.Path(), b.Path())
	})

	return files
}

func GetVideoFile(t *torrent.Torrent, episode int) (*torrent.File, error) {
	f := getSortedFilesList(t)[episode-1]
	ext := path.Ext(f.Path())
	switch ext {
	case ".mp4", ".mkv", ".avi", ".avif", ".av1", ".mov", ".flv", ".f4v", ".webm", ".wmv", ".mpeg", ".mpg", ".mlv", ".hevc", ".flac", ".flic":
		return f, nil
	default:
		return f, errors.New("server handler: Not supported extension")
	}
}

// handler for ServeTorrent
func (c *Client) handler(w http.ResponseWriter, r *http.Request) {
	ts := c.TorrentClient.Torrents()
	queries := r.URL.Query()
	// get hash of torrent
	hash := queries.Get("hash")
	// get episode
	ep, err := strconv.Atoi(queries.Get("ep"))
	if err != nil {
		http.Error(w, http.StatusText(400), http.StatusBadRequest)
		return
	}

	// idk why but this is always mangled af
	hash = strings.TrimSpace(hash)
	hash = strings.ReplaceAll(hash, "\n", "")

	if hash == "" {
		log.Println("server handler: Hash query is empty")
		return
	}

	for _, ff := range ts {
		<-ff.GotInfo()
		ih := ff.InfoHash().String()
		
		if ih == hash {
			f, err := GetVideoFile(ff, ep)
			if err != nil {
				log.Println(err)
				return
			}

			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, f.DisplayPath(), time.Unix(f.Torrent().Metainfo().CreationDate, 0), f.NewReader())
		}
	}
}

// start the server in the background
func (c *Client) StartServer() {
	// :8080 for localhost:8080/
	port := fmt.Sprintf(":%s", c.Port)
	c.srv = &http.Server{Addr: port}
	http.HandleFunc("/stream", c.handler)

	go func() {
		if err := c.srv.ListenAndServe(); err != nil {
			if err == http.ErrServerClosed {
				return
			} else {
				log.Fatal(err)
			}
		}
	}()
}

// Generate a link that can be used with the default clients server to play a torrent
// that is already loaded into the client
func (c *Client) ServeTorrent(t *torrent.Torrent, episode int) string {
	mh := t.InfoHash().String()
	return fmt.Sprintf("http://localhost:%s/stream?hash=%s&ep=%d", c.Port, mh, episode)
}

// returns a slice of loaded torrents or nil
func (c *Client) ShowTorrents() []*torrent.Torrent {
	return c.TorrentClient.Torrents()
}

// generic add torrent function
func (c *Client) AddTorrent(tor string) (*torrent.Torrent, error) {
	if strings.HasPrefix(tor, "magnet") {
		return c.AddMagnet(tor)
	} else if strings.Contains(tor, "http") {
		return c.AddTorrentURL(tor)
	} else {
		return c.AddTorrentFile(tor)
	}
}

func (c *Client) AddMagnet(magnet string) (*torrent.Torrent, error) {
	t, err := c.TorrentClient.AddMagnet(magnet)
	if err != nil {
		return nil, err
	}
	<-t.GotInfo()
	return t, nil
}

func (c *Client) AddTorrentFile(file string) (*torrent.Torrent, error) {
	t, err := c.TorrentClient.AddTorrentFromFile(file)
	if err != nil {
		return nil, err
	}
	<-t.GotInfo()
	return t, nil
}

func (c *Client) AddTorrentURL(url string) (*torrent.Torrent, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	fname := path.Base(url)
	tmp := os.TempDir()
	path.Join(tmp, fname)

	file, err := os.Create(fname)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return nil, err
	}

	t, err := c.TorrentClient.AddTorrentFromFile(file.Name())
	if err != nil {
		return nil, err
	}
	<-t.GotInfo()
	return t, nil
}

// stops the client and closes all connections to peers
func (c *Client) Close() (errs []error) {
	return c.TorrentClient.Close()
}

// look through the torrent files the client is handling and return a torrent with a
// matching info hash
func (c *Client) FindByInfoHhash(infoHash string) (*torrent.Torrent, error) {
	torrents := c.TorrentClient.Torrents()
	for _, t := range torrents {
		if t.InfoHash().AsString() == infoHash {
			return t, nil
		}
	}
	return nil, fmt.Errorf("No torrents match info hash: %v", infoHash)
}

func (c *Client) DropTorrent(t *torrent.Torrent) {
	t.Drop()
}

// Create storage path if it doesn't exist and return Path
func (c *Client) getStorage() (string, error) {
	s, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("Client error, couldnt get user cache directory: %v", err)
	}

	p := path.Join(s, c.Name)
	if p == "" || c.Name == "" {
		return "", fmt.Errorf("Client error, couldnt construct client path: Empty path or project name")
	}

	err = os.MkdirAll(p, 0o755)
	if err != nil {
		return "", fmt.Errorf("Client error, couldnt create project directory: %v", err)
	}

	_, err = os.Stat(p)
	if err == nil {
		return p, nil
	} else {
		return "", err
	}
}
