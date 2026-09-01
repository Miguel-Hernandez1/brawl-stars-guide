package apiclient

// PlayerResponse is returned by GET /players/{tag}.
// Only fields confirmed present in the official API are modeled here.
// Fields marked "uncertain" in docs/data-capabilities.md are left in raw_data (JSONB).
type PlayerResponse struct {
	Tag             string          `json:"tag"`
	Name            string          `json:"name"`
	Trophies        int             `json:"trophies"`
	HighestTrophies int             `json:"highestTrophies"`
	ExpLevel        int             `json:"expLevel"`
	ThreeVThreeWins int             `json:"3v3Victories"`
	SoloVictories   int             `json:"soloVictories"`
	DuoVictories    int             `json:"duoVictories"`
	Club            *PlayerClub     `json:"club"`
	Brawlers        []PlayerBrawler `json:"brawlers"`
}

type PlayerClub struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
}

type PlayerBrawler struct {
	ID              int           `json:"id"`
	Name            string        `json:"name"`
	Power           int           `json:"power"`
	Rank            int           `json:"rank"`
	Trophies        int           `json:"trophies"`
	HighestTrophies int           `json:"highestTrophies"`
	StarPowers      []StarPower   `json:"starPowers"`
	Gadgets         []Gadget      `json:"gadgets"`
	Gears           []Gear        `json:"gears"`
}

type StarPower struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Gadget struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Gear struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Level int    `json:"level"` // uncertain field; kept for JSONB fallback
}

// BattleLogResponse is returned by GET /players/{tag}/battlelog.
type BattleLogResponse struct {
	Items []BattleEntry `json:"items"`
}

// BattleEntry is one entry in a player's battle log.
type BattleEntry struct {
	// BattleTime is in Supercell's non-standard format: "20240901T143022.000Z"
	// Use apiclient.ParseBattleTime to convert.
	BattleTime string      `json:"battleTime"`
	Event      BattleEvent `json:"event"`
	Battle     BattleData  `json:"battle"`
}

type BattleEvent struct {
	ID   int    `json:"id"`
	Mode string `json:"mode"`
	Map  string `json:"map"`
}

type BattleData struct {
	Mode string `json:"mode"`
	// Type identifies the match type: "ranked", "soloRanked", "friendly", "challenge".
	// Use this to distinguish Ranked from trophy matches.
	Type string `json:"type"`
	// Result is the outcome from the perspective of the player whose log was fetched.
	// "victory", "defeat", or "draw". Absent in some showdown modes.
	Result string `json:"result"`
	// Duration is battle length in seconds. May be absent on some battle types.
	Duration int `json:"duration"`
	// TrophyChange is only present for the player whose log was fetched.
	// It is absent (nil) for all other participants.
	TrophyChange *int `json:"trophyChange"`
	// StarPlayer is the top performer, identified by tag. Not perspective-dependent.
	StarPlayer *BattleParticipant `json:"starPlayer"`
	// Teams is a 2D slice: [team_index][player_index].
	// For 3v3 modes: 2 teams of 3. For showdown: structure varies - verify on first batch.
	Teams [][]BattleParticipant `json:"teams"`
	// Rank is the finishing position in modes with rankings (e.g. showdown).
	Rank *int `json:"rank"`
}

type BattleParticipant struct {
	Tag    string              `json:"tag"`
	Name   string              `json:"name"`
	Brawler BrawlerInBattle    `json:"brawler"`
}

// BrawlerInBattle is the brawler snapshot within a battle log entry.
// Power and Trophies reflect the player's state at battle time per the API
// (verify this assumption against first 1000 battles - it may reflect current state).
type BrawlerInBattle struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Power    int    `json:"power"`
	Trophies int    `json:"trophies"`
}

// BrawlerListResponse is returned by GET /brawlers.
type BrawlerListResponse struct {
	Items []BrawlerItem `json:"items"`
}

type BrawlerItem struct {
	ID         int         `json:"id"`
	Name       string      `json:"name"`
	StarPowers []StarPower `json:"starPowers"`
	Gadgets    []Gadget    `json:"gadgets"`
}
