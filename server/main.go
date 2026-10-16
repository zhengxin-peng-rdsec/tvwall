package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFiles embed.FS

const (
	defaultListenAddr   = ":8088"
	onlineAfter         = 30 * time.Second
	maxFrameBytes       = 3 << 20
	defaultRefresh      = 5
	clientDiscoveryPort = 17301
)

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type Device struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	IP             string    `json:"ip"`
	Token          string    `json:"token"`
	URL            string    `json:"url"`
	RefreshSeconds int       `json:"refresh_seconds"`
	LastSeen       time.Time `json:"last_seen"`
	LastFrameAt    time.Time `json:"last_frame_at"`
	LastFrame      string    `json:"last_frame,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type Command struct {
	Type string `json:"type"`
}

type storeFile struct {
	Devices map[string]*Device `json:"devices"`
}

type Store struct {
	mu       sync.RWMutex
	devices  map[string]*Device
	commands map[string][]Command
	root     string
}

func newStore(root string) *Store {
	return &Store{devices: map[string]*Device{}, commands: map[string][]Command{}, root: root}
}

func (s *Store) load() error {
	b, err := os.ReadFile(filepath.Join(s.root, "devices.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var f storeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	if f.Devices != nil {
		s.devices = f.Devices
	}
	return nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(storeFile{Devices: s.devices}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.root, "devices.json.tmp")
	if err := os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.root, "devices.json"))
}

func randomToken() string {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("tv-%d", time.Now().UnixNano())
	}
	return "tv-" + base64.RawURLEncoding.EncodeToString(b)
}

func validDeviceID(id string) bool {
	return deviceIDPattern.MatchString(id)
}

// clientRemoteIP returns the address of the TCP peer. The client does not
// need to send or be configured with its own IP; every heartbeat updates it
// from the request that reached the server.
func clientRemoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func validClientIP(raw string) bool {
	return net.ParseIP(strings.TrimSpace(raw)) != nil
}

func validDisplayURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.ParseRequestURI(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 5<<20))
	return dec.Decode(dst)
}

func (s *Store) publicDevice(d *Device) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return publicDeviceFields(d)
}

func publicDeviceFields(d *Device) map[string]any {
	online := !d.LastSeen.IsZero() && time.Since(d.LastSeen) < onlineAfter
	copy := map[string]any{
		"id": d.ID, "name": d.Name, "ip": d.IP, "url": d.URL,
		"refresh_seconds": d.RefreshSeconds, "last_seen": d.LastSeen,
		"last_frame_at": d.LastFrameAt, "last_frame": d.LastFrame,
		"online": online,
	}
	return copy
}

func (s *Store) findClient(r *http.Request) (*Device, bool) {
	id := r.URL.Query().Get("device_id")
	token := r.URL.Query().Get("token")
	if id == "" || token == "" {
		return nil, false
	}
	s.mu.RLock()
	d, ok := s.devices[id]
	if ok && d.Token != token {
		ok = false
	}
	s.mu.RUnlock()
	return d, ok
}

func (s *Store) deviceList(w http.ResponseWriter) {
	s.mu.RLock()
	devices := make([]*Device, 0, len(s.devices))
	for _, d := range s.devices {
		devices = append(devices, d)
	}
	// A map has deliberately undefined iteration order. Keep the dashboard
	// layout stable by sorting devices by their enrollment time, with the ID as
	// a deterministic fallback for legacy records without CreatedAt.
	sort.SliceStable(devices, func(i, j int) bool {
		left, right := devices[i], devices[j]
		if left.CreatedAt.IsZero() != right.CreatedAt.IsZero() {
			return !left.CreatedAt.IsZero()
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})
	list := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		list = append(list, publicDeviceFields(d))
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"devices": list})
}

func (s *Store) createDevice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		IP             string `json:"ip"`
		URL            string `json:"url"`
		Token          string `json:"token"`
		RefreshSeconds int    `json:"refresh_seconds"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if in.ID != "" && !validDeviceID(in.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid device id"})
		return
	}
	in.IP = strings.TrimSpace(in.IP)
	if in.IP != "" && !validClientIP(in.IP) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid client ip"})
		return
	}
	if in.ID == "" && in.IP == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "client ip or device id is required"})
		return
	}
	if !validDisplayURL(in.URL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must use http or https"})
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		in.Name = "未命名电视"
	}
	if in.RefreshSeconds < 3 {
		in.RefreshSeconds = defaultRefresh
	}
	if in.ID == "" {
		in.ID = randomID()
	}
	if in.Token == "" {
		in.Token = randomToken()
	}
	now := time.Now().UTC()
	d := &Device{ID: in.ID, Name: in.Name, IP: in.IP, Token: in.Token, URL: in.URL, RefreshSeconds: in.RefreshSeconds, CreatedAt: now, UpdatedAt: now}
	s.mu.Lock()
	s.devices[d.ID] = d
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, s.publicDevice(d))
}

func (s *Store) deviceRoute(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/devices/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	id := parts[0]
	s.mu.RLock()
	d, ok := s.devices[id]
	s.mu.RUnlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "device not found"})
		return
	}
	if len(parts) > 1 && parts[1] == "command" && r.Method == http.MethodPost {
		var in Command
		if err := decodeJSON(r, &in); err != nil || in.Type == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid command"})
			return
		}
		s.mu.Lock()
		s.commands[id] = append(s.commands[id], in)
		s.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
		return
	}
	if len(parts) > 1 && parts[1] == "config" && r.Method == http.MethodPut {
		var in struct {
			Name           string `json:"name"`
			URL            string `json:"url"`
			RefreshSeconds int    `json:"refresh_seconds"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if !validDisplayURL(in.URL) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url must use http or https"})
			return
		}
		s.mu.Lock()
		if name := strings.TrimSpace(in.Name); name != "" {
			d.Name = name
		}
		d.URL = in.URL
		if in.RefreshSeconds >= 3 {
			d.RefreshSeconds = in.RefreshSeconds
		}
		d.UpdatedAt = time.Now().UTC()
		err := s.saveLocked()
		s.mu.Unlock()
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.publicDevice(d))
		return
	}
	if r.Method == http.MethodDelete {
		s.mu.Lock()
		delete(s.devices, id)
		delete(s.commands, id)
		err := s.saveLocked()
		s.mu.Unlock()
		_ = os.Remove(filepath.Join(s.root, "frames", id+".jpg"))
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

func (s *Store) registerClient(w http.ResponseWriter, r *http.Request) {
	var in struct{ ID, Name, Token string }
	if err := decodeJSON(r, &in); err != nil || in.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id is required"})
		return
	}
	if !validDeviceID(in.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid device id"})
		return
	}
	now := time.Now().UTC()
	remoteIP := clientRemoteIP(r)
	s.mu.Lock()
	d, ok := s.devices[in.ID]
	claimedID := ""
	if !ok {
		// A fixed-IP entry created by the administrator can be claimed by the
		// real client ID on its first registration. This keeps enrollment
		// possible without asking the operator to discover the Android ID.
		for id, candidate := range s.devices {
			if id != in.ID && candidate.LastSeen.IsZero() && candidate.IP != "" && candidate.IP == remoteIP {
				d, claimedID = candidate, id
				break
			}
		}
	}
	if !ok && d == nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "device is not enrolled; add its client IP first"})
		return
	}
	if claimedID != "" {
		delete(s.devices, claimedID)
		delete(s.commands, claimedID)
		d.ID = in.ID
		d.Token = randomToken()
		s.devices[in.ID] = d
		ok = true
	}
	// A device that has never completed a heartbeat may be re-enrolled from
	// its administrator-registered IP even if the APK still carries a token
	// from an earlier server or enrollment. The response returns the current
	// token, so the client can replace its cached credentials without a
	// reinstall.
	firstClaim := ok && d.LastSeen.IsZero() && d.IP != "" && d.IP == remoteIP
	if ok && d.Token != in.Token && !firstClaim && claimedID == "" {
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid device credentials"})
		return
	}
	if !ok {
		d = &Device{ID: in.ID, Name: strings.TrimSpace(in.Name), Token: in.Token, CreatedAt: now, RefreshSeconds: defaultRefresh}
		if d.Token == "" {
			d.Token = randomToken()
		}
		s.devices[in.ID] = d
	}
	// The client sends its model on every registration. Only use it as the
	// initial name; an operator-set name must survive subsequent heartbeats.
	if d.Name == "" && strings.TrimSpace(in.Name) != "" {
		d.Name = strings.TrimSpace(in.Name)
	}
	if remoteIP != "" {
		d.IP = remoteIP
	}
	d.LastSeen = now
	d.UpdatedAt = now
	err := s.saveLocked()
	commands := append([]Command(nil), s.commands[in.ID]...)
	s.commands[in.ID] = nil
	s.mu.Unlock()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"device_id": d.ID, "token": d.Token, "url": d.URL, "refresh_seconds": d.RefreshSeconds, "commands": commands})
}

func (s *Store) pollClient(w http.ResponseWriter, r *http.Request) {
	d, ok := s.findClient(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid device credentials"})
		return
	}
	now := time.Now().UTC()
	remoteIP := clientRemoteIP(r)
	s.mu.Lock()
	if remoteIP != "" {
		d.IP = remoteIP
	}
	d.LastSeen = now
	d.UpdatedAt = now
	commands := append([]Command(nil), s.commands[d.ID]...)
	s.commands[d.ID] = nil
	_ = s.saveLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"url": d.URL, "refresh_seconds": d.RefreshSeconds, "commands": commands})
}

func (s *Store) receiveFrame(w http.ResponseWriter, r *http.Request, immediate bool) {
	d, ok := s.findClient(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid device credentials"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFrameBytes)
	var in struct {
		Data string `json:"data"`
	}
	if err := decodeJSON(r, &in); err != nil || in.Data == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "data is required"})
		return
	}
	data := in.Data
	if idx := strings.Index(data, ","); idx >= 0 {
		data = data[idx+1:]
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(b) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image"})
		return
	}
	if len(b) > maxFrameBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "image too large"})
		return
	}
	frameDir := filepath.Join(s.root, "frames")
	if err := os.MkdirAll(frameDir, 0750); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	tmp := filepath.Join(frameDir, d.ID+".jpg.tmp")
	path := filepath.Join(frameDir, d.ID+".jpg")
	if err := os.WriteFile(tmp, b, 0640); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	now := time.Now().UTC()
	remoteIP := clientRemoteIP(r)
	s.mu.Lock()
	if remoteIP != "" {
		d.IP = remoteIP
	}
	d.LastSeen = now
	d.LastFrameAt = now
	d.LastFrame = "/frames/" + d.ID + ".jpg?v=" + strconv.FormatInt(now.Unix(), 10)
	d.UpdatedAt = now
	_ = s.saveLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "immediate": immediate})
}

func (s *Store) scan(w http.ResponseWriter, r *http.Request) {
	cidr := r.URL.Query().Get("cidr")
	if cidr == "" {
		cidr = "192.0.2.0/24"
	}
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid CIDR"})
		return
	}
	if ip.To4() == nil {
		writeJSON(w, 400, map[string]string{"error": "IPv4 CIDR required"})
		return
	}
	base := network.IP.To4()
	maskSize, bits := network.Mask.Size()
	hosts := 1 << uint(bits-maskSize)
	if hosts > 1024 {
		writeJSON(w, 400, map[string]string{"error": "scan range is limited to 1024 addresses"})
		return
	}
	type foundDevice struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		IP   string `json:"ip"`
	}
	found := make(chan foundDevice, hosts)
	var wg sync.WaitGroup
	for n := 1; n < hosts-1; n++ {
		candidate := make(net.IP, len(base))
		copy(candidate, base)
		candidate[3] = byte(int(base[3]) + n)
		addr := candidate.String()
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := http.Client{Timeout: 350 * time.Millisecond}
			resp, err := client.Get("http://" + net.JoinHostPort(addr, strconv.Itoa(clientDiscoveryPort)) + "/playbridge-discovery")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var v foundDevice
			if resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&v) == nil && v.ID != "" {
				v.IP = addr
				found <- v
			}
		}()
	}
	wg.Wait()
	close(found)
	results := []foundDevice{}
	for v := range found {
		results = append(results, v)
	}
	writeJSON(w, 200, map[string]any{"devices": results, "count": len(results)})
}

func (s *Store) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/devices", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			s.deviceList(w)
			return
		}
		if r.Method == http.MethodPost {
			s.createDevice(w, r)
			return
		}
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
	})
	mux.HandleFunc("/api/devices/", s.deviceRoute)
	mux.HandleFunc("/api/client/register", s.registerClient)
	mux.HandleFunc("/api/client/poll", s.pollClient)
	mux.HandleFunc("/api/client/frame", func(w http.ResponseWriter, r *http.Request) { s.receiveFrame(w, r, false) })
	mux.HandleFunc("/api/client/screenshot", func(w http.ResponseWriter, r *http.Request) { s.receiveFrame(w, r, true) })
	mux.HandleFunc("/api/scan", s.scan)
	frames := http.StripPrefix("/frames/", http.FileServer(http.Dir(filepath.Join(s.root, "frames"))))
	mux.Handle("/frames/", frames)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		b, err := webFiles.ReadFile("web/index.html")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "embedded web asset unavailable"})
			return
		}
		_, _ = w.Write(b)
	})
	return logging(mux)
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/client/poll" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

func main() {
	listenAddr := os.Getenv("TVWALL_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = defaultListenAddr
	}
	root := os.Getenv("TVWALL_DATA")
	if root == "" {
		root = "./data"
	}
	if err := os.MkdirAll(filepath.Join(root, "frames"), 0750); err != nil {
		log.Fatal(err)
	}
	s := newStore(root)
	if err := s.load(); err != nil {
		log.Fatal(err)
	}
	log.Printf("tvwall listening on %s, data=%s", listenAddr, root)
	if err := http.ListenAndServe(listenAddr, s.routes()); err != nil {
		log.Fatal(err)
	}
}
