package msgraphtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/msgraph/models"
	"github.com/gravitational/teleport/lib/msgraph/msgraphtest"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/stretchr/testify/require"
)

func TestSetUsers(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/users/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListUsersDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// add new user
	alice := defaultStorage.Users[msgraphtest.AliceID]
	fakeServer.SetUsers([]*models.User{alice})

	deltaLink, deltas = roundTrip[models.ListUsersDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.AliceID)
}

func TestDeleteUsers(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()

	// create alice user, group1 and add membership and ownership to group1.
	storage := msgraphtest.NewStorage()
	alice := defaultStorage.Users[msgraphtest.AliceID]
	storage.Users[msgraphtest.AliceID] = alice
	storage.Groups[msgraphtest.Group1ID] = defaultStorage.Groups[msgraphtest.Group1ID]
	storage.GroupMembers[msgraphtest.Group1ID] = []models.GroupMember{alice}
	storage.GroupOwners[msgraphtest.Group1ID] = []*models.User{alice}

	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/users/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	userDeltaLink, userDeltas := roundTrip[models.ListUsersDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, userDeltas)

	groupDeltaLatest := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)
	groupDeltaLink, groupDeltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, groupDeltaLatest, fakeServer.TLSServer.Client())
	require.Empty(t, groupDeltas)

	// delete user
	fakeServer.DeleteUsers([]string{*alice.GetID()})

	userDeltaLink, userDeltas = roundTrip[models.ListUsersDeltaResponse](t, ctx, userDeltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, userDeltaLink)

	require.Len(t, userDeltas, 1)
	require.Equal(t, *userDeltas[0].GetID(), msgraphtest.AliceID)

	groupDeltaLink, groupDeltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, groupDeltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, groupDeltaLink)

	require.Len(t, groupDeltas, 1)
	require.Equal(t, *groupDeltas[0].ID, msgraphtest.Group1ID)
	require.Len(t, groupDeltas[0].Members, 1)
	require.Equal(t, *groupDeltas[0].Members[0].ID, msgraphtest.AliceID)
	require.Equal(t, *groupDeltas[0].Members[0].Removed.Reason, "deleted")
	require.Len(t, groupDeltas[0].Owners, 1)
	require.Equal(t, *groupDeltas[0].Owners[0].ID, msgraphtest.AliceID)
	require.Equal(t, *groupDeltas[0].Owners[0].Removed.Reason, "deleted")
}

func TestSetGroup(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// add new group
	group1 := defaultStorage.Groups[msgraphtest.Group1ID]
	fakeServer.SetGroups([]*models.Group{group1})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.Group1ID)
}

func TestDeleteGroup(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()

	group1 := defaultStorage.Groups[msgraphtest.Group1ID]
	storage.Groups[msgraphtest.Group1ID] = group1

	storage.Groups[msgraphtest.Group2ID] = defaultStorage.Groups[msgraphtest.Group2ID]
	storage.GroupMembers[msgraphtest.Group2ID] = []models.GroupMember{group1}

	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// delete group
	fakeServer.DeleteGroups([]string{msgraphtest.Group1ID})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	// require.Len(t, deltas, 2)

	expected := []models.ListGroupsDeltaResponse{
		{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID: to.Ptr(msgraphtest.Group1ID),
				},
			},
			Removed: &models.RemovedReason{
				Reason: to.Ptr("deleted"),
			},
		},
		{
			Group: &models.Group{
				DirectoryObject: models.DirectoryObject{
					ID:          to.Ptr(string(msgraphtest.Group2ID)),
					DisplayName: to.Ptr("group2"),
				},
			},
			Members: []models.MembersDelta{
				{
					DirectoryObject: &models.DirectoryObject{
						ID: to.Ptr(msgraphtest.Group1ID),
					},
					Type: models.ODataGroup,
					Removed: &models.RemovedReason{
						Reason: to.Ptr("deleted"),
					},
				},
			},
		},
	}

	require.ElementsMatch(t, expected, deltas)
}

func TestSetGroupMember(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	storage.Groups[msgraphtest.Group1ID] = defaultStorage.Groups[msgraphtest.Group1ID]
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// add new group
	alice := defaultStorage.Users[msgraphtest.AliceID]
	fakeServer.SetGroupMembers(msgraphtest.Group1ID, []models.GroupMember{alice})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.Group1ID)
	require.Len(t, deltas[0].Members, 1)
	require.Equal(t, *deltas[0].Members[0].ID, *alice.ID)
}

func TestDeleteGroupMember(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	storage.Groups[msgraphtest.Group1ID] = defaultStorage.Groups[msgraphtest.Group1ID]

	alice := defaultStorage.Users[msgraphtest.AliceID]
	storage.GroupMembers[msgraphtest.Group1ID] = []models.GroupMember{alice}
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// delete group member

	fakeServer.DeleteGroupMembers(msgraphtest.Group1ID, []string{*alice.ID})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.Group1ID)
	require.Len(t, deltas[0].Members, 1)
	require.Equal(t, *deltas[0].Members[0].ID, *alice.ID)
	require.Equal(t, *deltas[0].Members[0].Removed.Reason, "deleted")
}

func TestSetGroupOwners(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	storage.Groups[msgraphtest.Group1ID] = defaultStorage.Groups[msgraphtest.Group1ID]
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// add new group
	alice := defaultStorage.Users[msgraphtest.AliceID]
	fakeServer.SetGroupOwners(msgraphtest.Group1ID, []*models.User{alice})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.Group1ID)
	require.Empty(t, deltas[0].Members)
	require.Len(t, deltas[0].Owners, 1)
	require.Equal(t, *deltas[0].Owners[0].ID, *alice.ID)
}

func TestDeleteGroupOwners(t *testing.T) {
	defaultStorage := msgraphtest.NewDefaultStorage()
	storage := msgraphtest.NewStorage()
	storage.Groups[msgraphtest.Group1ID] = defaultStorage.Groups[msgraphtest.Group1ID]

	alice := defaultStorage.Users[msgraphtest.AliceID]
	storage.GroupOwners[msgraphtest.Group1ID] = []*models.User{alice}
	fakeServer := msgraphtest.NewServer(msgraphtest.WithStorage(storage))

	ctx := t.Context()
	url := fmt.Sprintf("%s/v1.0/groups/delta?$deltatoken=latest", fakeServer.TLSServer.URL)

	deltaLink, deltas := roundTrip[models.ListGroupsDeltaResponse](t, ctx, url, fakeServer.TLSServer.Client())
	require.Empty(t, deltas)

	// delete group member

	fakeServer.DeleteGroupOwners(msgraphtest.Group1ID, []string{*alice.ID})

	deltaLink, deltas = roundTrip[models.ListGroupsDeltaResponse](t, ctx, deltaLink, fakeServer.TLSServer.Client())
	require.NotEmpty(t, deltaLink)

	require.Len(t, deltas, 1)
	require.Equal(t, *deltas[0].GetID(), msgraphtest.Group1ID)
	require.Empty(t, deltas[0].Members)
	require.Len(t, deltas[0].Owners, 1)
	require.Equal(t, *deltas[0].Owners[0].ID, *alice.ID)
	require.Equal(t, *deltas[0].Owners[0].Removed.Reason, "deleted")
}

func roundTrip[T any](t *testing.T, ctx context.Context, url string, client *http.Client) (string, []T) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	require.NoError(t, err)

	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := utils.ReadAtMost(resp.Body, teleport.MaxHTTPResponseSize)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode, "expected response status to match")

	var odata models.ODataPage
	err = json.Unmarshal(respBody, &odata)
	require.NoError(t, err)

	var out []T
	err = json.Unmarshal(odata.Value, &out)
	require.NoError(t, err, "expected valid delta response object")

	return odata.DeltaLink, out
}
