package server

import (
	"net/http"
	"strconv"

	"github.com/NullAngst/Fleetling/internal/store"
)

type actionLogData struct {
	Target  string
	Actions []store.Action
	Page    int
	More    bool
}

const actionsPerPage = 100

func (s *Server) actionLog(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	target := r.URL.Query().Get("target")
	list, err := s.store.ListActions(r.Context(), target, actionsPerPage+1, (page-1)*actionsPerPage)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := actionLogData{Target: target, Page: page}
	if len(list) > actionsPerPage {
		d.More, list = true, list[:actionsPerPage]
	}
	d.Actions = list
	s.render(w, http.StatusOK, "actions", s.page(r, "Action log", "actions", d))
}
