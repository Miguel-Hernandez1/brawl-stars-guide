// api is the Brawl Stars Playbook REST API binary.
//
// Usage:
//
//	api                   - start HTTP server (reads DATABASE_URL and API_ADDR from env)
//
// Environment variables:
//
//	DATABASE_URL  - PostgreSQL connection string (required)
//	API_ADDR      - listen address (default: :8080)
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/analytics"
	"github.com/Miguel-Hernandez1/brawl-stars-playbook/internal/storage"
)

func main() {
	ctx := context.Background()

	pool, err := storage.Connect(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/meta/winrates", winRatesHandler(pool))
	mux.HandleFunc("GET /v1/meta/maps", mapsHandler(pool))

	addr := envOr("API_ADDR", ":8080")
	log.Printf("api: listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// parseBattleType reads the battle_type query param, validates it, and returns
// the corresponding BattleTypeFilter. Returns BattleTypeRanked if the param is
// absent. Writes a 400 and returns false if the value is unrecognized.
func parseBattleType(w http.ResponseWriter, q map[string][]string) (analytics.BattleTypeFilter, bool) {
	raw := ""
	if vals, ok := q["battle_type"]; ok && len(vals) > 0 {
		raw = vals[0]
	}
	if raw == "" {
		return analytics.BattleTypeRanked, true
	}
	bt := analytics.BattleTypeFilter(raw)
	if !analytics.ValidBattleType(bt) {
		writeErr(w, http.StatusBadRequest,
			"battle_type must be one of: ranked, soloRanked, competitive, any")
		return "", false
	}
	return bt, true
}

// -- winrates --

// winRateResponse is the JSON envelope for GET /v1/meta/winrates.
// data_label values per field:
//   DIRECT:  sample_battles, total_slots, brawler.battles, brawler.slots, brawler.wins, brawler.losses, brawler.draws
//   DERIVED: brawler.win_pct (wins/(wins+losses)), brawler.pick_rate (slots/total_slots)
type winRateResponse struct {
	Mode                string               `json:"mode"`
	Map                 *string              `json:"map,omitempty"`
	BattleType          string               `json:"battle_type"`
	BrawlerTrophyBucket *int16               `json:"brawler_trophy_bucket,omitempty"`
	MinBattles          int                  `json:"min_battles"`
	DataLabel           string               `json:"data_label"`
	SampleBattles       int                  `json:"sample_battles"`
	TotalSlots          int                  `json:"total_slots"`
	Brawlers            []brawlerWinRateJSON `json:"brawlers"`
}

type brawlerWinRateJSON struct {
	BrawlerID int      `json:"brawler_id"`
	Name      string   `json:"name"`
	Battles   int      `json:"battles"`
	Slots     int      `json:"slots"`
	Wins      int      `json:"wins"`
	Losses    int      `json:"losses"`
	Draws     int      `json:"draws"`
	WinPct    *float64 `json:"win_pct"`
	PickRate  *float64 `json:"pick_rate"`
}

func winRatesHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		mode := q.Get("mode")
		if mode == "" {
			writeErr(w, http.StatusBadRequest, "mode is required")
			return
		}
		if analytics.IsIneligibleMode(mode) {
			writeErr(w, http.StatusBadRequest,
				"mode does not support binary win-rate (rank-based outcomes or no brawler attribution)")
			return
		}

		bt, ok := parseBattleType(w, q)
		if !ok {
			return
		}

		params := analytics.WinRateParams{
			Mode:       mode,
			BattleType: bt,
			MinBattles: 10,
		}

		if raw := q.Get("map"); raw != "" {
			params.Map = &raw
		}

		if raw := q.Get("brawler_trophy_bucket"); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 16)
			if err != nil || n < 1 || n > 6 {
				writeErr(w, http.StatusBadRequest, "brawler_trophy_bucket must be an integer 1-6")
				return
			}
			b := int16(n)
			params.BrawlerTrophyBucket = &b
		}

		if raw := q.Get("min_battles"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeErr(w, http.StatusBadRequest, "min_battles must be a positive integer")
				return
			}
			params.MinBattles = n
		}

		result, err := analytics.BrawlerWinRates(r.Context(), pool, params)
		if err != nil {
			log.Printf("win rates query mode=%s: %v", mode, err)
			writeErr(w, http.StatusInternalServerError, "internal server error")
			return
		}

		resp := winRateResponse{
			Mode:                mode,
			Map:                 params.Map,
			BattleType:          string(params.BattleType),
			BrawlerTrophyBucket: params.BrawlerTrophyBucket,
			MinBattles:          params.MinBattles,
			DataLabel:           "DIRECT+DERIVED",
			SampleBattles:       result.SampleBattles,
			TotalSlots:          result.TotalSlots,
			Brawlers:            make([]brawlerWinRateJSON, len(result.Brawlers)),
		}
		for i, b := range result.Brawlers {
			resp.Brawlers[i] = brawlerWinRateJSON{
				BrawlerID: b.BrawlerID,
				Name:      b.Name,
				Battles:   b.Battles,
				Slots:     b.Slots,
				Wins:      b.Wins,
				Losses:    b.Losses,
				Draws:     b.Draws,
				WinPct:    b.WinPct,
				PickRate:  b.PickRate,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("encode response: %v", err)
		}
	}
}

// -- maps --

type mapsResponse struct {
	Mode       string         `json:"mode"`
	BattleType string         `json:"battle_type"`
	DataLabel  string         `json:"data_label"`
	Maps       []mapEntryJSON `json:"maps"`
}

type mapEntryJSON struct {
	Map                string `json:"map"`
	Battles            int    `json:"battles"`
	QualifyingBrawlers int    `json:"qualifying_brawlers"`
}

func mapsHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		mode := q.Get("mode")
		if mode == "" {
			writeErr(w, http.StatusBadRequest, "mode is required")
			return
		}
		if analytics.IsIneligibleMode(mode) {
			writeErr(w, http.StatusBadRequest,
				"mode does not support map list (rank-based outcomes or no brawler attribution)")
			return
		}

		bt, ok := parseBattleType(w, q)
		if !ok {
			return
		}

		result, err := analytics.MapList(r.Context(), pool, analytics.MapListParams{
			Mode:       mode,
			BattleType: bt,
		})
		if err != nil {
			log.Printf("map list query mode=%s: %v", mode, err)
			writeErr(w, http.StatusInternalServerError, "internal server error")
			return
		}

		resp := mapsResponse{
			Mode:       mode,
			BattleType: string(bt),
			DataLabel:  "DIRECT",
			Maps:       make([]mapEntryJSON, len(result.Maps)),
		}
		for i, m := range result.Maps {
			resp.Maps[i] = mapEntryJSON{
				Map:                m.Map,
				Battles:            m.Battles,
				QualifyingBrawlers: m.QualifyingBrawlers,
			}
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("encode response: %v", err)
		}
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s is not set", key)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
