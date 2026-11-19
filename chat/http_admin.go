package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Provider admin request/response shapes. They are separate from the chat wire
// types so the admin surface can evolve without touching the user API.

type providerListView struct {
	Items         []ProviderView `json:"items"`
	DefaultChoice string         `json:"defaultChoice,omitempty"`
	// TitleChoice is "auto", "off" or a choice id; absent when the admin
	// does not manage a title model.
	TitleChoice string `json:"titleChoice,omitempty"`
}

type setTitleRequest struct {
	Mode     string `json:"mode"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type titleChoiceView struct {
	TitleChoice string `json:"titleChoice"`
}

type providerView struct {
	Provider ProviderView `json:"provider"`
}

type deletedView struct {
	Deleted bool `json:"deleted"`
}

type setDefaultRequest struct {
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
}

type defaultChoiceView struct {
	DefaultChoice string `json:"defaultChoice,omitempty"`
}

type refreshModelsRequest struct {
	Save bool `json:"save"`
}

type modelsView struct {
	Models []ModelOption `json:"models"`
}

type signInCompleteRequest struct {
	URL string `json:"url"`
}

type importCredentialsRequest struct {
	Credentials json.RawMessage `json:"credentials"`
}

// adminFailure maps a provider admin failure onto the chat error envelope.
// Implementations return *AdminError with an explicit status; anything else is
// a safe validation failure, never a raw internal error.
func (h *handler) adminFailure(w http.ResponseWriter, err error) {
	var ae *AdminError
	if errors.As(err, &ae) {
		code := ae.Code
		switch ae.Status {
		case http.StatusUnauthorized:
			code = CodeUnauthorized
		case http.StatusForbidden:
			code = CodeForbidden
		case http.StatusNotFound:
			code = CodeNotFound
		case http.StatusConflict:
			code = CodeConflict
		case http.StatusInternalServerError:
			code = CodeInternal
		}
		if code == "" {
			code = CodeInvalidRequest
		}
		message := ae.Message
		if message == "" {
			message = msgInvalidRequest
		}
		if ae.Status >= 500 {
			// Never echo an internal error, even from a service that forgot to
			// sanitize its own message.
			message = msgInternal
		}
		h.writeError(w, ae.Status, code, message)
		return
	}
	h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
}

func (h *handler) adminCaller(w http.ResponseWriter, r *http.Request) (Caller, bool) {
	return h.requireCaller(w, r)
}

func (h *handler) listProviders(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	items, defaultChoice, err := h.cfg.ProviderAdmin.ListProviders(r.Context())
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	if items == nil {
		items = []ProviderView{}
	}
	view := providerListView{Items: items, DefaultChoice: defaultChoice}
	if titles, ok := h.cfg.ProviderAdmin.(TitleAdmin); ok {
		choice, err := titles.TitleChoice(r.Context())
		if err != nil {
			h.adminFailure(w, err)
			return
		}
		view.TitleChoice = choice
	}
	h.writeJSON(w, http.StatusOK, view)
}

func (h *handler) setTitleChoice(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	titles, ok := h.cfg.ProviderAdmin.(TitleAdmin)
	if !ok {
		h.notFound(w, r)
		return
	}
	var req setTitleRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if err := titles.SetTitleChoice(r.Context(), mode, strings.TrimSpace(req.Provider), strings.TrimSpace(req.Model)); err != nil {
		h.adminFailure(w, err)
		return
	}
	choice, err := titles.TitleChoice(r.Context())
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, titleChoiceView{TitleChoice: choice})
}

func (h *handler) createProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var in ProviderInput
	if !h.decodeBody(w, r, false, &in) {
		return
	}
	view, err := h.cfg.ProviderAdmin.SaveProvider(r.Context(), "", in)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusCreated, providerView{Provider: view})
}

func (h *handler) getProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	view, err := h.cfg.ProviderAdmin.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, providerView{Provider: view})
}

func (h *handler) updateProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var in ProviderInput
	if !h.decodeBody(w, r, false, &in) {
		return
	}
	view, err := h.cfg.ProviderAdmin.SaveProvider(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, providerView{Provider: view})
}

func (h *handler) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	if err := h.cfg.ProviderAdmin.DeleteProvider(r.Context(), r.PathValue("id")); err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, deletedView{Deleted: true})
}

func (h *handler) testProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	result, err := h.cfg.ProviderAdmin.TestProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, result)
}

func (h *handler) testProviderConfig(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var in ProviderInput
	if !h.decodeBody(w, r, false, &in) {
		return
	}
	result, err := h.cfg.ProviderAdmin.TestProviderConfig(r.Context(), in)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, result)
}

func (h *handler) refreshProviderModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var req refreshModelsRequest
	if r.Body != nil && r.ContentLength != 0 {
		if !h.decodeBody(w, r, true, &req) {
			return
		}
	}
	models, err := h.cfg.ProviderAdmin.RefreshProviderModels(r.Context(), r.PathValue("id"), req.Save)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	if models == nil {
		models = []ModelOption{}
	}
	h.writeJSON(w, http.StatusOK, modelsView{Models: models})
}

func (h *handler) setDefaultProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var req setDefaultRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	providerID, model := "", ""
	if req.Provider != nil {
		providerID = strings.TrimSpace(*req.Provider)
	}
	if req.Model != nil {
		model = strings.TrimSpace(*req.Model)
	}
	if err := h.cfg.ProviderAdmin.SetDefaultChoice(r.Context(), providerID, model); err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, defaultChoiceView{DefaultChoice: ChoiceID(providerID, model)})
}

func (h *handler) beginProviderSignIn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.adminCaller(w, r)
	if !ok {
		return
	}
	start, err := h.cfg.ProviderAdmin.BeginSignIn(r.Context(), r.PathValue("id"), caller.Owner.UserID)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, start)
}

func (h *handler) completeProviderSignIn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.adminCaller(w, r)
	if !ok {
		return
	}
	var req signInCompleteRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	view, err := h.cfg.ProviderAdmin.CompleteSignIn(r.Context(), r.PathValue("id"), caller.Owner.UserID, req.URL)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, providerView{Provider: view})
}

func (h *handler) cancelProviderSignIn(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.adminCaller(w, r)
	if !ok {
		return
	}
	if err := h.cfg.ProviderAdmin.CancelSignIn(r.Context(), r.PathValue("id"), caller.Owner.UserID); err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, deletedView{Deleted: true})
}

func (h *handler) signOutProvider(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	if err := h.cfg.ProviderAdmin.SignOut(r.Context(), r.PathValue("id")); err != nil {
		h.adminFailure(w, err)
		return
	}
	view, err := h.cfg.ProviderAdmin.GetProvider(r.Context(), r.PathValue("id"))
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, providerView{Provider: view})
}

func (h *handler) importProviderCredentials(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.adminCaller(w, r); !ok {
		return
	}
	var req importCredentialsRequest
	if !h.decodeBody(w, r, false, &req) {
		return
	}
	if len(req.Credentials) == 0 {
		h.writeError(w, http.StatusBadRequest, CodeInvalidRequest, msgInvalidRequest)
		return
	}
	view, err := h.cfg.ProviderAdmin.ImportCredentials(r.Context(), r.PathValue("id"), req.Credentials)
	if err != nil {
		h.adminFailure(w, err)
		return
	}
	h.writeJSON(w, http.StatusOK, providerView{Provider: view})
}
