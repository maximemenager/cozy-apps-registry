package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/flimzy/kivik/driver/couchdb/chttp"
)

const (
	viewsHelpers = `
function getVersionChannel(version) {
  if (version.indexOf("-dev.") >= 0) {
    return "dev";
  }
  if (version.indexOf("-beta.") >= 0) {
    return "beta";
  }
  return "stable";
}

function expandVersion(doc) {
  var v = [];
  var exp = 0;
  var sp = doc.version.split(".");
  v[0] = parseInt(sp[0], 10);
  v[1] = parseInt(sp[1], 10);
  var channel = getVersionChannel(doc.version);
  if (channel == "stable") {
    v[2] = parseInt(sp[2], 10);
  } else if (channel == "beta") {
    sp = sp[2].split("-beta.");
    v[2] = parseInt(sp[0]);
    exp = sp[1];
  } else if (channel == "dev") {
    sp = sp[2].split("-dev.");
    v[2] = parseInt(sp[0]);
  }
  return {
    v: v,
    channel: channel,
    code: (channel == "stable") ? 1 : 0,
    exp: exp,
    date: doc.created_at,
  };
}`

	devView = `
function(doc) {
  ` + viewsHelpers + `
  var version = expandVersion(doc);
  var key = version.v.concat(version.code, +new Date(version.date))
  emit(key, doc.version);
}`

	betaView = `
function(doc) {
  ` + viewsHelpers + `
  var version = expandVersion(doc);
  var channel = version.channel;
  if (channel == "beta" || channel == "stable") {
    var key = version.v.concat(version.code, version.exp)
    emit(key, doc.version);
  }
}`

	stableView = `
function(doc) {
  ` + viewsHelpers + `
  var version = expandVersion(doc);
  var channel = version.channel;
  if (channel == "stable") {
    var key = version.v;
    emit(key, doc.version);
  }
}`
)

type view struct {
	Map string `json:"map"`
}

var versionsViews = map[string]view{
	"dev":    {Map: devView},
	"beta":   {Map: betaView},
	"stable": {Map: stableView},
}

func versViewDocName(appName string) string {
	return "versions-" + appName
}

func createVersionsViews(appName string) error {
	return createViews(VersDB, versViewDocName(appName), versionsViews)
}

func createViews(dbName, ddoc string, views map[string]view) error {
	chttpClient, err := chttp.New(ctx, clientURL.String())
	if err != nil {
		return err
	}

	var object struct {
		Rev   string `json:"_rev"`
		Views map[string]view
	}

	ddocID := fmt.Sprintf("_design/%s", url.PathEscape(ddoc))
	path := fmt.Sprintf("/%s/%s", dbName, ddocID)

	var viewsBodies []string
	for name, view := range views {
		viewsBodies = append(viewsBodies,
			string(sprintfJSON(`%s: {"map": %s}`, name, view.Map)))
	}

	viewsBody := `{` + strings.Join(viewsBodies, ",") + `}`

	body, _ := json.Marshal(struct {
		ID       string          `json:"_id"`
		Rev      string          `json:"_rev,omitempty"`
		Views    json.RawMessage `json:"views"`
		Language string          `json:"language"`
	}{
		ID:       ddocID,
		Rev:      object.Rev,
		Views:    json.RawMessage(viewsBody),
		Language: "javascript",
	})

	_, err = chttpClient.DoError(ctx, http.MethodPut, path, &chttp.Options{
		Body: bytes.NewReader(body),
	})
	return err
}
