package app

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Boards are created and reconfigured with the moderation key rather than from
// a migration, so a deployment can open a board without a release. Listing them
// stays public; everything that writes goes through authorizeMod, the same gate
// the thread actions use.

// Bounds on a board's fields. The slug appears in every URL and in the board
// navigation, so it is kept short and to the characters that survive a path
// segment untouched.
const (
	maxBoardSlugLen     = 24
	maxBoardNameLen     = 64
	maxBoardDescLen     = 256
	maxBoardRulesLen    = 4000
	maxBoardBumpLimit   = 10000
	maxBoardThreadLimit = 1000
	maxBoardImageLimit  = 1000
	// maxBoardArchiveDays bounds how long a board can keep archived threads,
	// and with them their images. It is a sanity bound, not a retention policy.
	maxBoardArchiveDays = 365
)

// Defaults for a board created without explicit limits. They mirror the column
// defaults in 0002_imageboard.sql and 0005_archive_and_limits.sql so an
// API-created board matches a seeded one.
const (
	defaultBoardBumpLimit   = 300
	defaultBoardThreadLimit = 100
	defaultBoardImageLimit  = 150
	defaultBoardArchiveDays = 7
)

// boardSlugPattern allows lowercase alphanumerics in dash-separated groups:
// /general/ and /comp-sci/ are fine, "-x", "x-", and "a--b" are not.
var boardSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// reservedBoardSlugs are the first path segments the server already answers on.
// A board using one would be reachable through the API but not through the UI,
// whose board route is /{slug}/.
var reservedBoardSlugs = map[string]bool{
	"api":     true,
	"media":   true,
	"healthz": true,
	"verify":  true,
}

type createBoardRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Rules       string `json:"rules"`
	// Limits are optional; omitting one takes the default rather than zero,
	// which is why they are pointers.
	BumpLimit   *int `json:"bump_limit"`
	ThreadLimit *int `json:"thread_limit"`
	ImageLimit  *int `json:"image_limit"`
	ArchiveDays *int `json:"archive_days"`
}

func (a *App) handleCreateBoard(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeMod(w, r) {
		return
	}
	var req createBoardRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	board := Board{
		BumpLimit:   defaultBoardBumpLimit,
		ThreadLimit: defaultBoardThreadLimit,
		ImageLimit:  defaultBoardImageLimit,
		ArchiveDays: defaultBoardArchiveDays,
	}
	var ok bool
	if board.Slug, ok = boardSlug(w, req.Slug); !ok {
		return
	}
	// A board is usable with nothing but a slug; the name then reads as the
	// slug does, which is what most boards are called anyway.
	name := req.Name
	if strings.TrimSpace(name) == "" {
		name = board.Slug
	}
	if board.Name, ok = boardName(w, name); !ok {
		return
	}
	if board.Description, ok = boardDescription(w, req.Description); !ok {
		return
	}
	if board.Rules, ok = boardRules(w, req.Rules); !ok {
		return
	}
	if req.BumpLimit != nil {
		if board.BumpLimit, ok = boardLimit(w, "bump_limit", *req.BumpLimit, 1, maxBoardBumpLimit); !ok {
			return
		}
	}
	if req.ThreadLimit != nil {
		if board.ThreadLimit, ok = boardLimit(w, "thread_limit", *req.ThreadLimit, 1, maxBoardThreadLimit); !ok {
			return
		}
	}
	if req.ImageLimit != nil {
		if board.ImageLimit, ok = boardLimit(w, "image_limit", *req.ImageLimit, 0, maxBoardImageLimit); !ok {
			return
		}
	}
	if req.ArchiveDays != nil {
		if board.ArchiveDays, ok = boardLimit(w, "archive_days", *req.ArchiveDays, 0, maxBoardArchiveDays); !ok {
			return
		}
	}

	created, err := a.store.CreateBoard(r.Context(), board)
	if errors.Is(err, errConflict) {
		writeError(w, http.StatusConflict, "board /"+board.Slug+"/ already exists")
		return
	}
	if err != nil {
		a.fail(w, r, err, "could not create board")
		return
	}
	a.log.Info("moderation", "action", "create board", "board", created.Slug)
	writeJSON(w, http.StatusCreated, created)
}

// updateBoardRequest is a partial update: an omitted field keeps its stored
// value, which is why every field is a pointer.
type updateBoardRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Rules       *string `json:"rules"`
	BumpLimit   *int    `json:"bump_limit"`
	ThreadLimit *int    `json:"thread_limit"`
	ImageLimit  *int    `json:"image_limit"`
	ArchiveDays *int    `json:"archive_days"`
}

func (a *App) handleUpdateBoard(w http.ResponseWriter, r *http.Request) {
	if !a.authorizeMod(w, r) {
		return
	}
	var req updateBoardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req == (updateBoardRequest{}) {
		writeError(w, http.StatusBadRequest, "no board settings given")
		return
	}

	var settings BoardSettings
	if req.Name != nil {
		name, ok := boardName(w, *req.Name)
		if !ok {
			return
		}
		settings.Name = &name
	}
	if req.Description != nil {
		description, ok := boardDescription(w, *req.Description)
		if !ok {
			return
		}
		settings.Description = &description
	}
	if req.Rules != nil {
		rules, ok := boardRules(w, *req.Rules)
		if !ok {
			return
		}
		settings.Rules = &rules
	}
	if req.BumpLimit != nil {
		limit, ok := boardLimit(w, "bump_limit", *req.BumpLimit, 1, maxBoardBumpLimit)
		if !ok {
			return
		}
		settings.BumpLimit = &limit
	}
	if req.ThreadLimit != nil {
		limit, ok := boardLimit(w, "thread_limit", *req.ThreadLimit, 1, maxBoardThreadLimit)
		if !ok {
			return
		}
		settings.ThreadLimit = &limit
	}
	if req.ImageLimit != nil {
		limit, ok := boardLimit(w, "image_limit", *req.ImageLimit, 0, maxBoardImageLimit)
		if !ok {
			return
		}
		settings.ImageLimit = &limit
	}
	if req.ArchiveDays != nil {
		days, ok := boardLimit(w, "archive_days", *req.ArchiveDays, 0, maxBoardArchiveDays)
		if !ok {
			return
		}
		settings.ArchiveDays = &days
	}

	// A lowered thread limit is applied by the next post to the board rather
	// than here, which is the same pruning path every other post takes.
	updated, err := a.store.UpdateBoard(r.Context(), r.PathValue("slug"), settings)
	if err != nil {
		a.fail(w, r, err, "could not update board")
		return
	}
	a.log.Info("moderation", "action", "update board", "board", updated.Slug)
	writeJSON(w, http.StatusOK, updated)
}

// boardSlug validates the identifier a board is addressed by. It writes the
// error response itself and reports success.
func boardSlug(w http.ResponseWriter, raw string) (string, bool) {
	slug := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case slug == "":
		writeError(w, http.StatusBadRequest, "slug is required")
	case utf8.RuneCountInString(slug) > maxBoardSlugLen:
		writeError(w, http.StatusBadRequest,
			"slug must be at most "+strconv.Itoa(maxBoardSlugLen)+" characters")
	case !boardSlugPattern.MatchString(slug):
		writeError(w, http.StatusBadRequest,
			"slug must be lowercase letters and digits, in groups separated by single dashes")
	case reservedBoardSlugs[slug]:
		writeError(w, http.StatusBadRequest, "slug "+slug+" is reserved")
	default:
		return slug, true
	}
	return "", false
}

func boardName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(collapseControl(strings.ReplaceAll(raw, "\n", " ")))
	switch {
	case name == "":
		writeError(w, http.StatusBadRequest, "name must not be empty")
	case utf8.RuneCountInString(name) > maxBoardNameLen:
		writeError(w, http.StatusBadRequest,
			"name must be at most "+strconv.Itoa(maxBoardNameLen)+" characters")
	default:
		return name, true
	}
	return "", false
}

// boardDescription accepts an empty description; a board does not need one.
func boardDescription(w http.ResponseWriter, raw string) (string, bool) {
	description := strings.TrimSpace(collapseControl(strings.ReplaceAll(raw, "\n", " ")))
	if utf8.RuneCountInString(description) > maxBoardDescLen {
		writeError(w, http.StatusBadRequest,
			"description must be at most "+strconv.Itoa(maxBoardDescLen)+" characters")
		return "", false
	}
	return description, true
}

// boardRules accepts multi-line text, normalised the way a post body is, and
// an empty string, which clears a board's own rules.
func boardRules(w http.ResponseWriter, raw string) (string, bool) {
	rules := normalizeBody(raw)
	if utf8.RuneCountInString(rules) > maxBoardRulesLen {
		writeError(w, http.StatusBadRequest,
			"rules must be at most "+strconv.Itoa(maxBoardRulesLen)+" characters")
		return "", false
	}
	return rules, true
}

// boardLimit bounds a board's numeric settings. None of them has an
// "unlimited" value: the bump and thread limits take a minimum of one, because
// a zero bump limit would freeze every thread on the board, while a zero image
// limit or archive period means exactly what it says — no images in replies,
// no time in the archive.
func boardLimit(w http.ResponseWriter, field string, value, minimum, maximum int) (int, bool) {
	if value < minimum || value > maximum {
		writeError(w, http.StatusBadRequest,
			field+" must be between "+strconv.Itoa(minimum)+" and "+strconv.Itoa(maximum))
		return 0, false
	}
	return value, true
}
