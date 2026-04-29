// Teleport
// Copyright (C) 2025 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package msgraphtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/lib/msgraph/models"
)

// Server defines fake server.
type Server struct {
	mu        sync.RWMutex
	TLSServer *httptest.Server
	Storage   *Storage
}

// ServerOption is a custom opt for [NewServer].
type ServerOption func(*Server)

// WithStorage configures default storage
func WithStorage(storage *Storage) ServerOption {
	return func(s *Server) {
		s.Storage = storage
	}
}

// NewServer creates a new fake server.
func NewServer(opts ...ServerOption) *Server {
	// By default, use storage populated with default mock data
	s := &Server{
		Storage: NewDefaultStorage(),
	}
	// Apply options
	for _, opt := range opts {
		opt(s)
	}

	s.TLSServer = httptest.NewTLSServer(s.Handler())

	return s
}

// Fake server handler
func (s *Server) Handler() http.Handler {
	r := http.NewServeMux()

	r.HandleFunc("GET /v1.0/users", s.handleListUsers)
	r.HandleFunc("GET /v1.0/users/delta", s.handleListUsersDelta)
	r.HandleFunc("GET /v1.0/groups", s.handleListGroups)
	r.HandleFunc("GET /v1.0/groups/delta", s.handleListGroupsDelta)
	r.HandleFunc("GET /v1.0/groups/{id}/members", s.handleListGroupMembers)
	r.HandleFunc("GET /v1.0/groups/{id}/owners/microsoft.graph.user", s.handleListGroupOwners)
	r.HandleFunc("/v1.0/", s.handleCatchAll)
	r.HandleFunc("/metadata/identity/oauth2/token", s.handleGetToken)

	return r
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	users := make([]*models.User, 0, len(s.Storage.Users))
	for _, user := range s.Storage.Users {
		users = append(users, user)
	}
	s.mu.RUnlock()

	jsonResponse(w, map[string]interface{}{
		"value": users,
	})
}

// handleListUsersDelta handles delta queries.
// It expects a sequential delta key and values based on
// incremental requests. It does not support pagination.
// It consumes existing token on each request, increments delta token
// counter by one and responds with the new delta token.
// SetUsers and DeleteUsers methods configure delta values.
func (s *Server) handleListUsersDelta(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := r.URL.Query().Get("$deltatoken")
	currentKey := 0
	var users []models.ListUsersDeltaResponse

	switch token {
	case "latest":
		// latest request is the starting point.
		users = make([]models.ListUsersDeltaResponse, 0)
	default:
		i, err := parseToken(token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		currentKey = i
		users = append(users, s.Storage.UsersDelta[i]...)
	}

	currentKey++
	if _, ok := s.Storage.UsersDelta[currentKey]; !ok {
		s.Storage.UsersDelta[currentKey] = []models.ListUsersDeltaResponse{}
	}

	if len(users) == 0 {
		users = []models.ListUsersDeltaResponse{}
	}

	jsonResponse(w, map[string]interface{}{
		"@odata.deltaLink": deltaLink(r, strconv.Itoa(currentKey)),
		"value":            users,
	})
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	groups := make([]*models.Group, 0, len(s.Storage.Groups))
	for _, group := range s.Storage.Groups {
		groups = append(groups, group)
	}

	jsonResponse(w, map[string]interface{}{
		"value": groups,
	})
}

// handleListGroupsDelta handles group delta queries.
// It expects a sequential delta key and values based on
// incremental requests. It does not support pagination.
// It consumes existing token on each request, increments delta token
// counter by one and responds with the new delta token.
// SetGroups, DeleteGroups, SetGroupOwners, DeleteGroupOwners
// SetGroupMembers, DeleteGroupMembers methods configure gorup delta values.
func (s *Server) handleListGroupsDelta(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := r.URL.Query().Get("$deltatoken")
	currentKey := 0
	var groups []models.ListGroupsDeltaResponse

	switch token {
	case "latest":
		groups = make([]models.ListGroupsDeltaResponse, 0)
	default:
		i, err := parseToken(token)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		currentKey = i
		groups = append(groups, s.Storage.GroupsDelta[i]...)
	}

	currentKey++
	if _, ok := s.Storage.GroupsDelta[currentKey]; !ok {
		s.Storage.GroupsDelta[currentKey] = []models.ListGroupsDeltaResponse{}
	}

	if len(groups) == 0 {
		groups = []models.ListGroupsDeltaResponse{}
	}

	jsonResponse(w, map[string]interface{}{
		"@odata.deltaLink": deltaLink(r, strconv.Itoa(currentKey)),
		"value":            groups,
	})
}

func (s *Server) handleListGroupMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groupID := r.PathValue("id")
	groupMembers := s.Storage.GroupMembers[groupID]

	members := make([]map[string]interface{}, 0, len(groupMembers))
	for _, member := range groupMembers {
		memberData := map[string]interface{}{
			"id": member.GetID(),
		}

		switch member.(type) {
		case *models.User:
			memberData["@odata.type"] = "#microsoft.graph.user"
		case *models.Group:
			memberData["@odata.type"] = "#microsoft.graph.group"
		default:
			// Default to user if unknown
			memberData["@odata.type"] = "#microsoft.graph.user"
		}

		members = append(members, memberData)
	}

	jsonResponse(w, map[string]interface{}{
		"value": members,
	})
}

func (s *Server) handleListGroupOwners(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	groupID := r.PathValue("id")
	owners := s.Storage.GroupOwners[groupID]

	jsonResponse(w, map[string]interface{}{
		"value": owners,
	})
}

// handleGetApplication handles GET /v1.0/applications(appId='...') requests.
func (s *Server) handleGetApplication(w http.ResponseWriter, r *http.Request, appID string) {
	s.mu.RLock()
	app, ok := s.Storage.Applications[appID]
	s.mu.RUnlock()

	if !ok {
		http.Error(w, "application not found", http.StatusNotFound)
		return
	}

	jsonResponse(w, app)
}

var (
	applicationByAppIDPattern = regexp.MustCompile(`^/v1\.0/applications\(appId='([^']+)'\)$`)
)

// handleCatchAll handles other endpoints like applications(appId='app-id').
func (s *Server) handleCatchAll(w http.ResponseWriter, r *http.Request) {
	// Handle GET /v1.0/applications(appId='app-id')
	if r.Method == http.MethodGet {
		if matches := applicationByAppIDPattern.FindStringSubmatch(r.URL.Path); matches != nil {
			appID := matches[1]
			s.handleGetApplication(w, r, appID)
			return
		}
	}

	http.NotFound(w, r)
}

// handleGetToken handles token request.
func (s *Server) handleGetToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	// credential detail is irrelevant.
	const token = `{
		"token_type": "Bearer",
		"scope": "Mail.Read User.Read",
		"expires_in": 3600,
		"ext_expires_in": 3600,
		"access_token": "abc-access-token",
		"refresh_token": "abc-refresh-token"
	}`
	w.Write([]byte(token))
}

// SetUsers updates users storage.
func (s *Server) SetUsers(users []*models.User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, user := range users {
		if user.ID != nil {
			s.Storage.Users[*user.ID] = user
		}
	}

	// update user delta
	var userDelta []models.ListUsersDeltaResponse
	for _, d := range users {
		userDelta = append(userDelta, models.ListUsersDeltaResponse{
			User: d,
		})
	}

	appendUserDeltas(s.Storage, userDelta...)
}

// DeleteUsers removes users from the storage.
func (s *Server) DeleteUsers(users []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, userID := range users {
		if userID != "" {
			delete(s.Storage.Users, userID)
		}
	}

	// Update user delta
	var userDelta []models.ListUsersDeltaResponse
	for _, userID := range users {
		userDelta = append(userDelta, models.ListUsersDeltaResponse{
			User: &models.User{
				DirectoryObject: models.DirectoryObject{
					ID: to.Ptr(userID),
				},
			},
			Removed: &models.RemovedReason{
				Reason: to.Ptr("deleted"),
			},
		})
	}

	appendUserDeltas(s.Storage, userDelta...)

	for _, userID := range users {
		s.deleteGroupMemberships(userID)
		s.deleteGroupOwnerships(userID)
	}
}

// SetGroups updates groups storage.
func (s *Server) SetGroups(groups []*models.Group) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, group := range groups {
		if group.ID != nil {
			s.Storage.Groups[*group.ID] = group
		}
	}

	// Update group deltas.
	var groupDeltas []models.ListGroupsDeltaResponse
	for _, g := range groups {
		groupDeltas = append(groupDeltas, models.ListGroupsDeltaResponse{
			Group: g,
		})
	}
	appendGroupDeltas(s.Storage, groupDeltas...)
}

func (s *Server) deleteGroupMemberships(memberID string) {
	// deleteGroupMemberships expects caller to hold the lock.
	for gid := range s.Storage.GroupMembers {
		s.deleteGroupMembers(gid, []string{memberID})
	}
}

func (s *Server) deleteGroupOwnerships(ownerID string) {
	// deleteGroupOwnerships expects the caller to hold the lock.
	for gid := range s.Storage.GroupOwners {
		s.deleteGroupOwners(gid, []string{ownerID})
	}
}

// DeleteGroups deletes the groups from storage.
func (s *Server) DeleteGroups(groups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, groupID := range groups {
		if groupID == "" {
			continue
		}
		delete(s.Storage.Groups, groupID)
		delete(s.Storage.GroupMembers, groupID)
		delete(s.Storage.GroupOwners, groupID)
	}

	// update group delta
	var groupDeltas []models.ListGroupsDeltaResponse
	for _, groupID := range groups {
		groupDeltas = append(groupDeltas, models.ListGroupsDeltaResponse{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID: to.Ptr(groupID),
				},
			},
			Removed: &models.RemovedReason{
				Reason: to.Ptr("deleted"),
			},
		})

	}

	appendGroupDeltas(s.Storage, groupDeltas...)

	for _, groupID := range groups {
		s.deleteGroupMemberships(groupID)
	}
}

// SetGroupMembers updates group members storage.
func (s *Server) SetGroupMembers(groupID string, members []models.GroupMember) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingMembers := make(map[string]struct{})
	for _, m := range s.Storage.GroupMembers[groupID] {
		if m.GetID() == nil {
			continue
		}
		existingMembers[*m.GetID()] = struct{}{}
	}
	allMembers := slices.Concat(s.Storage.GroupMembers[groupID], members)
	s.Storage.GroupMembers[groupID] = utils.DeduplicateAny(allMembers,
		func(m1, m2 models.GroupMember) bool {
			if m1.GetID() == nil || m2.GetID() == nil {
				return false
			}

			return *m1.GetID() == *m2.GetID()
		})

	var memberDeltas []models.MembersDelta
	for _, m := range members {
		if m.GetID() == nil {
			continue
		}
		if _, ok := existingMembers[*m.GetID()]; ok {
			// This may be a no op if member already exists.
			continue
		}
		existingMembers[*m.GetID()] = struct{}{}
		switch m.(type) {
		case *models.User:
			memberDeltas = append(memberDeltas, models.MembersDelta{
				DirectoryObject: &models.DirectoryObject{
					ID: m.GetID(),
				},
				Type: "#microsoft.graph.user",
			})
		case *models.Group:
			memberDeltas = append(memberDeltas, models.MembersDelta{
				DirectoryObject: &models.DirectoryObject{
					ID: m.GetID(),
				},
				Type: "#microsoft.graph.group",
			})
		default:
			continue
		}
	}

	if len(memberDeltas) == 0 {
		return
	}

	group, ok := s.Storage.Groups[groupID]
	if !ok {
		// should never happen
		return
	}

	latestKey := latestDeltaKey(s.Storage.GroupsDelta)
	deltas := s.Storage.GroupsDelta[latestKey]
	found := false
	for i, d := range deltas {
		if d.Group == nil || d.Group.GetID() == nil {
			continue
		}
		if *d.Group.GetID() == groupID {
			found = true
			d.Members = append(d.Members, memberDeltas...)
			deltas[i] = d
		}
	}
	if found {
		s.Storage.GroupsDelta[latestKey] = deltas
		return
	}
	appendGroupDeltas(s.Storage, models.ListGroupsDeltaResponse{
		Group: &models.Group{
			DirectoryObject: models.DirectoryObject{
				ID:          to.Ptr(groupID),
				DisplayName: group.DisplayName,
			},
		},
		Members: memberDeltas,
	})
}

// DeleteGroupMembers removes group memberships.
func (s *Server) DeleteGroupMembers(groupID string, members []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleteGroupMembers(groupID, members)
}

func (s *Server) deleteGroupMembers(groupID string, members []string) {
	groupMembers := s.Storage.GroupMembers[groupID]
	newMembers := []models.GroupMember{}
	newMembersDeltas := []models.MembersDelta{}
	for _, gm := range groupMembers {
		if gm.GetID() == nil {
			continue
		}
		if slices.Contains(members, *gm.GetID()) {
			// only expecting owner of user type.
			newMembersDeltas = append(newMembersDeltas, models.MembersDelta{
				DirectoryObject: &models.DirectoryObject{
					ID: gm.GetID(),
				},
				Removed: &models.RemovedReason{
					Reason: to.Ptr("deleted"),
				},
				Type: memberType(gm),
			})
		} else {
			newMembers = append(newMembers, gm)
		}
	}
	s.Storage.GroupMembers[groupID] = newMembers

	if len(newMembersDeltas) == 0 {
		return
	}

	// Update delta
	latestKey := latestDeltaKey(s.Storage.GroupsDelta)

	group, ok := s.Storage.Groups[groupID]
	if !ok {
		// should never happen
		return
	}

	deltas := s.Storage.GroupsDelta[latestKey]
	found := false
	for i, d := range deltas {
		if d.Group == nil || d.Group.GetID() == nil {
			continue
		}
		if *d.Group.GetID() == groupID {
			found = true
			d.Members = append(d.Members, newMembersDeltas...)
			deltas[i] = d
		}
	}
	if found {
		s.Storage.GroupsDelta[latestKey] = deltas
	} else {
		appendGroupDeltas(s.Storage, models.ListGroupsDeltaResponse{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID:          to.Ptr(groupID),
					DisplayName: group.DisplayName,
				},
			},
			Members: newMembersDeltas,
		})
	}
}

// SetGroupOwners updates group owners storage.
func (s *Server) SetGroupOwners(groupID string, owners []*models.User) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existingOwners := make(map[string]struct{})
	for _, m := range s.Storage.GroupOwners[groupID] {
		if m.GetID() == nil {
			continue
		}
		existingOwners[*m.GetID()] = struct{}{}
	}
	allowners := slices.Concat(s.Storage.GroupOwners[groupID], owners)
	s.Storage.GroupOwners[groupID] = utils.DeduplicateAny(allowners,
		func(m1, m2 *models.User) bool {
			if m1.GetID() == nil || m2.GetID() == nil {
				return false
			}
			return *m1.GetID() == *m2.GetID()
		})

	var ownerDeltas []models.OwnersDelta
	for _, o := range owners {
		if o.GetID() == nil {
			continue
		}
		if _, ok := existingOwners[*o.GetID()]; ok {
			continue
		}
		existingOwners[*o.GetID()] = struct{}{}
		ownerDeltas = append(ownerDeltas, models.OwnersDelta{
			User: &models.User{
				DirectoryObject: models.DirectoryObject{
					ID: o.GetID(),
				},
			},
			Type: "#microsoft.graph.user",
		})
	}

	if len(ownerDeltas) == 0 {
		return
	}

	group, ok := s.Storage.Groups[groupID]
	if !ok {
		// should never happen
		return
	}

	latestKey := latestDeltaKey(s.Storage.GroupsDelta)
	deltas := s.Storage.GroupsDelta[latestKey]
	found := false
	for i, d := range deltas {
		if d.Group == nil || d.Group.GetID() == nil {
			continue
		}
		if *d.Group.GetID() == groupID {
			found = true
			d.Owners = append(d.Owners, ownerDeltas...)
			deltas[i] = d
		}
	}

	if found {
		s.Storage.GroupsDelta[latestKey] = deltas
	} else {
		appendGroupDeltas(s.Storage, models.ListGroupsDeltaResponse{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID:          to.Ptr(groupID),
					DisplayName: group.DisplayName,
				},
			},
			Owners: ownerDeltas,
		})
	}
}

// DeleteGroupOwners removes group ownership.
func (s *Server) DeleteGroupOwners(groupID string, owners []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.deleteGroupOwners(groupID, owners)
}

func (s *Server) deleteGroupOwners(groupID string, owners []string) {
	groupOwners := s.Storage.GroupOwners[groupID]
	newOwners := []*models.User{}
	deletedOwnersDelta := []models.OwnersDelta{}
	for _, o := range groupOwners {
		if o.GetID() == nil {
			continue
		}
		if slices.Contains(owners, *o.GetID()) {
			// only expecting owner of user type.
			deletedOwnersDelta = append(deletedOwnersDelta, models.OwnersDelta{
				User: &models.User{
					DirectoryObject: models.DirectoryObject{
						ID: o.GetID(),
					},
				},
				Removed: &models.RemovedReason{
					Reason: to.Ptr("deleted"),
				},
				Type: "#microsoft.graph.user",
			})
		} else {
			newOwners = append(newOwners, o)
		}
	}
	s.Storage.GroupOwners[groupID] = newOwners

	if len(deletedOwnersDelta) == 0 {
		return
	}

	// Update delta
	latestKey := latestDeltaKey(s.Storage.GroupsDelta)

	group, ok := s.Storage.Groups[groupID]
	if !ok {
		// should never happen
		return
	}

	deltas := s.Storage.GroupsDelta[latestKey]
	found := false
	for i, d := range deltas {
		if d.Group == nil || d.Group.GetID() == nil {
			continue
		}
		if *d.Group.GetID() == groupID {
			found = true
			d.Owners = append(d.Owners, deletedOwnersDelta...)
			deltas[i] = d
		}
	}
	if found {
		s.Storage.GroupsDelta[latestKey] = deltas
	} else {
		appendGroupDeltas(s.Storage, models.ListGroupsDeltaResponse{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID:          to.Ptr(groupID),
					DisplayName: group.DisplayName,
				},
			},
			Owners: deletedOwnersDelta,
		})
	}
}

// SetApplications updates application storage.
func (s *Server) SetApplications(apps []*models.Application) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, app := range apps {
		if app.AppID != nil {
			s.Storage.Applications[*app.AppID] = app
		}
	}
}

func jsonResponse(writer http.ResponseWriter, data interface{}) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(data); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

// RewriteTransport configures custom transport.
type RewriteTransport struct {
	Base http.RoundTripper
	URL  *url.URL
}

// RoundTrip swaps incoming URL with configured URL.
func (rt *RewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = rt.URL.Scheme
	req.URL.Host = rt.URL.Host
	return rt.Base.RoundTrip(req)
}

// FakeDeltaStore implements [DeltaStore].
type FakeDeltaStore struct {
	mu    sync.Mutex
	cache map[string]string
}

// NewFakeDeltaStore creates a new [FakeDeltaStore].
func NewFakeDeltaStore() *FakeDeltaStore {
	return &FakeDeltaStore{
		cache: make(map[string]string),
	}
}

// Get returns delta token for the given endpoint.
func (s *FakeDeltaStore) Get(endpoint string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[endpoint]
}

// Set sets delta token for the given endpoint.
func (s *FakeDeltaStore) Set(endpoint, link string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[endpoint] = link
}

// Clear removes delta token for the given endpoint.
func (s *FakeDeltaStore) Clear(endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, endpoint)
}

func memberType(gm models.GroupMember) string {
	memberType := "#microsoft.graph.user"
	switch gm.(type) {
	case *models.Group:
		memberType = "#microsoft.graph.group"
	default:
		// handle unknown member type
	}

	return memberType
}

func appendUserDeltas(s *Storage, deltas ...models.ListUsersDeltaResponse) {
	key := latestDeltaKey(s.UsersDelta)
	s.UsersDelta[key] = append(s.UsersDelta[key], deltas...)
}

func appendGroupDeltas(s *Storage, deltas ...models.ListGroupsDeltaResponse) {
	key := latestDeltaKey(s.GroupsDelta)
	s.GroupsDelta[key] = append(s.GroupsDelta[key], deltas...)
}

func parseToken(token string) (int, error) {
	parts := strings.Split(token, "#") // delta token counter is separated with #
	if len(parts) != 2 {
		return 0, trace.BadParameter("invalid delta token")
	}
	if parts[1] == "" {
		return 0, trace.BadParameter("invalid delta token")
	}

	return strconv.Atoi(parts[1])
}

func latestDeltaKey[T any](deltaMap map[int][]T) int {
	latest := 0
	for key := range deltaMap {
		if key > latest {
			latest = key
		}
	}
	return latest
}

func deltaLink(r *http.Request, deltaToken string) string {
	u := url.URL{
		Host:     r.Host,
		Scheme:   "https",
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
	}
	q := u.Query()
	q.Del("$deltatoken")
	q.Set("$deltatoken", "fake-deltatoken#"+deltaToken)
	u.RawQuery = q.Encode()
	return u.String()
}
