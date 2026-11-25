package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// fakeServer answers every request with an empty list and records what it was sent.
type fakeServer struct {
	requests []string
}

func (f *fakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests = append(f.requests, req.Method+" "+req.URL.RequestURI())

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"type": "sync", "status": "Success", "status_code": 200, "metadata": []}`)),
		Request:    req,
	}

	return resp, nil
}

func TestRestrictToReadOnly(t *testing.T) {
	fake := &fakeServer{}

	server, err := incus.ConnectIncusHTTP(&incus.ConnectionArgs{SkipGetServer: true}, &http.Client{Transport: fake})
	require.NoError(t, err)

	log := &bytes.Buffer{}
	require.NoError(t, restrictToReadOnly(server, log))

	// Reads go through and are logged, including on targeted connections.
	_, err = server.GetProjects()
	require.NoError(t, err)

	_, err = server.UseTarget("node1").GetClusterGroups()
	require.NoError(t, err)

	// Every kind of write is refused before reaching the server.
	err = server.CreateProject(api.ProjectsPost{Name: "foo"})
	require.ErrorContains(t, err, "Refusing to send POST")

	err = server.UpdateClusterMember("node1", api.ClusterMemberPut{}, "")
	require.ErrorContains(t, err, "Refusing to send PUT")

	err = server.UseTarget("node1").DeleteProject("foo")
	require.ErrorContains(t, err, "Refusing to send DELETE")

	_, err = server.GetEvents()
	require.Error(t, err)

	expected := []string{"GET /1.0/projects?recursion=1", "GET /1.0/cluster/groups?recursion=1&target=node1"}
	require.Equal(t, expected, fake.requests)
	require.Equal(t, strings.Join(expected, "\n")+"\n", log.String())
}
