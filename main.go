package main

import (
	"bufio"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"github.com/getlantern/systray"
)

//go:embed web/index.html
//go:embed icon.ico
var webFS embed.FS

type MapInfo struct {
	MapName        string   `json:"mapname"`
	CourseName     string   `json:"coursename"`
	CourseID       int      `json:"courseid"`
	WorkshopID     string   `json:"workshopid"`
	CkzNubTier     int      `json:"ckznubtier"`
	CkzProTier     int      `json:"ckzprotier"`
	VnlNubTier     int      `json:"vnlnubtier"`
	VnlProTier     int      `json:"vnlprotier"`
	ImageURL       string   `json:"image"`
	BestOverallCkz *float64 `json:"bestOverallCkz,omitempty"`
	BestProCkz     *float64 `json:"bestProCkz,omitempty"`
	BestOverallVnl *float64 `json:"bestOverallVnl,omitempty"`
	BestProVnl     *float64 `json:"bestProVnl,omitempty"`
}

var (
	mapsCache   []MapInfo
	cacheMutex  sync.RWMutex
	lastFetch   time.Time
	fetchPeriod = 5 * time.Minute
	db          *sql.DB
	bestMu      sync.RWMutex
)

const jsonURL = "https://raw.githubusercontent.com/YesSeir/cs2kz-maps/main/maps.json"

func getSteamPath() string {
	log.Println("Searching for Steam path...")
	if runtime.GOOS == "windows" {
		cmd := exec.Command("reg", "query", `HKCU\Software\Valve\Steam`, "/v", "SteamPath")
		out, err := cmd.Output()
		if err == nil {
			lines := strings.Split(string(out), "\n")
			for _, line := range lines {
				if strings.Contains(line, "SteamPath") {
					re := regexp.MustCompile(`REG_(?:EXPAND_)?SZ\s+(.+)`)
					matches := re.FindStringSubmatch(line)
					if len(matches) > 1 {
						path := strings.TrimSpace(matches[1])
						return path
					}
				}
			}
		}
		log.Println("Steam path not found via registry")
		return ""
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "linux" {
		path := filepath.Join(home, ".steam", "steam")
		log.Printf("Steam path (Linux): %s", path)
		return path
	}
	if runtime.GOOS == "darwin" {
		path := filepath.Join(home, "Library", "Application Support", "Steam")
		log.Printf("Steam path (macOS): %s", path)
		return path
	}
	return ""
}

func findDBPath() string {
	if env := os.Getenv("CS2KZ_DB_PATH"); env != "" {
		log.Printf("Checking env path: %s", env)
		if _, err := os.Stat(env); err == nil {
			log.Printf("Database found at env: %s", env)
			return env
		}
	}

	steamPath := getSteamPath()
	if steamPath == "" {
		log.Println("Steam path not found")
		return ""
	}

	vdfPath := filepath.Join(steamPath, "steamapps", "libraryfolders.vdf")
	log.Printf("Reading VDF: %s", vdfPath)

	file, err := os.Open(vdfPath)
	if err != nil {
		log.Printf("Cannot open VDF: %v", err)
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var depth int
	var currentBlockPath string
	var hasCS2 bool
	var inBlock bool

	rePath := regexp.MustCompile(`^"path"\s+"(.+)"`)
	reBlock := regexp.MustCompile(`^"(\d+)"$`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		if strings.Contains(line, "{") {
			depth++
		}
		if strings.Contains(line, "}") {
			depth--
		}

		if depth == 1 && reBlock.MatchString(line) {
			if inBlock && hasCS2 && currentBlockPath != "" {
				dbPath := filepath.Join(currentBlockPath, "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo", "addons", "cs2kz", "data", "cs2kz.sqlite3")
				log.Printf("Checking DB: %s", dbPath)
				if _, err := os.Stat(dbPath); err == nil {
					log.Printf("Database found: %s", dbPath)
					return dbPath
				}
			}

			blockID := strings.Trim(line, `"`)
			currentBlockPath = ""
			hasCS2 = false
			inBlock = true
			log.Printf("Starting library block: %s", blockID)
			continue
		}

		if !inBlock {
			continue
		}

		if depth == 2 {
			if matches := rePath.FindStringSubmatch(line); len(matches) > 1 {
				currentBlockPath = matches[1]
				log.Printf("Found path for block: %s", currentBlockPath)
			}
		}

		if strings.Contains(line, `"730"`) {
			hasCS2 = true
			log.Printf("Found CS2 (AppID 730) in block")
		}

		if depth == 1 && inBlock && strings.Contains(line, "}") {
			if hasCS2 && currentBlockPath != "" {
				dbPath := filepath.Join(currentBlockPath, "steamapps", "common", "Counter-Strike Global Offensive", "game", "csgo", "addons", "cs2kz", "data", "cs2kz.sqlite3")
				log.Printf("Checking DB on block close: %s", dbPath)
				if _, err := os.Stat(dbPath); err == nil {
					log.Printf("Database found: %s", dbPath)
					return dbPath
				}
			}
			inBlock = false
			currentBlockPath = ""
			hasCS2 = false
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("VDF scan error: %v", err)
	}

	log.Println("Database not found in any Steam library")
	return ""
}

func initDB() {
	log.Println("Initializing database...")
	dbPath := findDBPath()
	if dbPath == "" {
		log.Println("Database not found. Best times will not be shown.")
		return
	}
	var err error
	db, err = sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		log.Printf("Failed to open DB: %v", err)
		return
	}
	if err := db.Ping(); err != nil {
		log.Printf("DB ping failed: %v", err)
		db.Close()
		db = nil
		return
	}
	log.Printf("Database connected: %s", dbPath)
	if err := loadBestTimes(); err != nil {
		log.Printf("Failed to load best times: %v", err)
	}
}

func loadBestTimes() error {
	bestMu.Lock()
	defer bestMu.Unlock()
	if db == nil {
		log.Println("loadBestTimes: db is nil")
		return nil
	}
	log.Println("Loading best times from DB...")
	queries := []struct {
		modeID int
		isPro  bool
		label  string
	}{
		{2, false, "CKZ Overall"},
		{2, true, "CKZ Pro"},
		{1, false, "VNL Overall"},
		{1, true, "VNL Pro"},
	}
	tempBest := make(map[string]*float64)
	for _, q := range queries {
		proCondition := ""
		if q.isPro {
			proCondition = "AND t.Teleports = 0"
		}
		query := fmt.Sprintf(`
			SELECT m.Name, mc.StageID, MIN(t.RunTime)
			FROM Times t
			JOIN MapCourses mc ON t.MapCourseID = mc.ID
			JOIN Maps m ON mc.MapID = m.ID
			WHERE t.RunTime > 0 AND t.ModeID = %d %s
			GROUP BY m.Name, mc.StageID
		`, q.modeID, proCondition)
		log.Printf("Executing query for %s", q.label)
		rows, err := db.Query(query)
		if err != nil {
			log.Printf("Query error for %s: %v", q.label, err)
			continue
		}
		count := 0
		for rows.Next() {
			var mapName string
			var courseID int
			var best float64
			if err := rows.Scan(&mapName, &courseID, &best); err != nil {
				log.Printf("Scan error: %v", err)
				continue
			}
			key := fmt.Sprintf("%s|%d|%d|%t", mapName, courseID, q.modeID, q.isPro)
			val := best
			tempBest[key] = &val
			count++
		}
		rows.Close()
		log.Printf("Loaded %d records for %s", count, q.label)
	}
	log.Printf("Total best time records loaded: %d", len(tempBest))
	cacheMutex.Lock()
	defer cacheMutex.Unlock()
	for i := range mapsCache {
		m := &mapsCache[i]
		keyOverallCkz := fmt.Sprintf("%s|%d|%d|%t", m.MapName, m.CourseID, 2, false)
		keyProCkz := fmt.Sprintf("%s|%d|%d|%t", m.MapName, m.CourseID, 2, true)
		keyOverallVnl := fmt.Sprintf("%s|%d|%d|%t", m.MapName, m.CourseID, 1, false)
		keyProVnl := fmt.Sprintf("%s|%d|%d|%t", m.MapName, m.CourseID, 1, true)
		if v, ok := tempBest[keyOverallCkz]; ok {
			m.BestOverallCkz = v
		}
		if v, ok := tempBest[keyProCkz]; ok {
			m.BestProCkz = v
		}
		if v, ok := tempBest[keyOverallVnl]; ok {
			m.BestOverallVnl = v
		}
		if v, ok := tempBest[keyProVnl]; ok {
			m.BestProVnl = v
		}
	}
	return nil
}

func fetchMaps() error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(jsonURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var maps []MapInfo
	if err := json.Unmarshal(body, &maps); err != nil {
		return err
	}
	cacheMutex.Lock()
	mapsCache = maps
	lastFetch = time.Now()
	cacheMutex.Unlock()
	if db != nil {
		go loadBestTimes()
	}
	log.Printf("Loaded %d maps", len(maps))
	return nil
}

func getMaps() []MapInfo {
	cacheMutex.RLock()
	defer cacheMutex.RUnlock()
	return mapsCache
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	html, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Write(html)
}

func iconHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	ico, err := webFS.ReadFile("icon.ico")
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Write(ico)
}

func apiMapsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Query().Get("refresh") == "1" {
		go func() {
			if err := fetchMaps(); err != nil {
				log.Printf("Refresh error: %v", err)
			}
		}()
	}
	maps := getMaps()
	json.NewEncoder(w).Encode(maps)
}

func onReady() {
	systray.SetTitle("cs2kz-app")
	systray.SetTooltip("cs2kz-app")
	if ico, err := webFS.ReadFile("icon.ico"); err == nil {
		systray.SetIcon(ico)
	}
	mOpen := systray.AddMenuItem("Open", "Open in browser")
	mExit := systray.AddMenuItem("Exit", "Exit")
	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser("http://localhost:7777")
			case <-mExit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	if db != nil {
		db.Close()
	}
	os.Exit(0)
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		log.Printf("Failed to open browser: %v", err)
	}
}

func startServer() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/icon.ico", iconHandler)
	http.HandleFunc("/api/maps", apiMapsHandler)
	addr := "localhost:7777"
	log.Printf("Server started at http://%s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func main() {
	initDB()
	if err := fetchMaps(); err != nil {
		log.Fatalf("Failed to load maps: %v", err)
	}
	go func() {
		for {
			time.Sleep(fetchPeriod)
			if err := fetchMaps(); err != nil {
				log.Printf("Background map update error: %v", err)
			}
		}
	}()
	go startServer()
	go func() {
		time.Sleep(2 * time.Second)
		openBrowser("http://localhost:7777")
	}()
	systray.Run(onReady, onExit)
}