package registry

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/cozy/cozy-registry-v3/auth"
	"github.com/cozy/cozy-registry-v3/errshttp"
	multierror "github.com/hashicorp/go-multierror"

	"github.com/flimzy/kivik"
	_ "github.com/flimzy/kivik/driver/couchdb" // for couchdb
	"github.com/labstack/echo"
)

const maxApplicationSize = 20 * 1024 * 1024 // 20 Mo

var (
	validSlugReg    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9\-]*$`)
	validVersionReg = regexp.MustCompile(`^(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})\.(0|[1-9][0-9]{0,4})(-dev\.[a-f0-9]{1,40}|-beta.(0|[1-9][0-9]{0,4}))?$`)

	validAppTypes = []string{"webapp", "konnector"}
)

var (
	ErrAppNotFound     = errshttp.NewError(http.StatusNotFound, "Application was not found")
	ErrAppSlugMismatch = errshttp.NewError(http.StatusBadRequest, "Application slug does not match the one specified in the body")
	ErrAppInvalid      = errshttp.NewError(http.StatusBadRequest, "Invalid application name: should contain only alphanumeric characters and dashes")

	ErrVersionAlreadyExists = errshttp.NewError(http.StatusConflict, "Version already exists")
	ErrVersionNotFound      = errshttp.NewError(http.StatusNotFound, "Version was not found")
	ErrVersionMismatch      = errshttp.NewError(http.StatusBadRequest, "Version does not match the one specified in the body")
	ErrVersionInvalid       = errshttp.NewError(http.StatusBadRequest, "Invalid version value")
	ErrChannelInvalid       = errshttp.NewError(http.StatusBadRequest, `Invalid version channel: should be "stable", "beta" or "dev"`)
)

var versionClient = http.Client{
	Timeout: 20 * time.Second,
}

const (
	devSuffix  = "-dev."
	betaSuffix = "-beta."
)

var (
	AppsDB    = "registry-apps"
	VersDB    = "registry-versions"
	EditorsDB = "registry-editors"
)

var (
	client    *kivik.Client
	clientURL *url.URL

	ctx = context.Background()

	appsIndexes = map[string]echo.Map{
		"by-slug":       {"fields": []string{"slug"}},
		"by-type":       {"fields": []string{"type", "slug", "category"}},
		"by-editor":     {"fields": []string{"editor", "slug", "category"}},
		"by-category":   {"fields": []string{"category", "slug", "editor"}},
		"by-created_at": {"fields": []string{"created_at", "slug", "category", "editor"}},
		"by-updated_at": {"fields": []string{"updated_at", "slug", "category", "editor"}},
	}

	versIndex = echo.Map{"fields": []string{"version", "slug", "type"}}
)

type Channel int

const (
	Stable Channel = iota
	Beta
	Dev
)

type App struct {
	ID             string          `json:"_id,omitempty"`
	Rev            string          `json:"_rev,omitempty"`
	Slug           string          `json:"slug"`
	Name           *AppName        `json:"name"`
	Type           string          `json:"type"`
	Editor         string          `json:"editor"`
	Developer      *Developer      `json:"developer"`
	Description    *AppDescription `json:"description"`
	Category       string          `json:"category"`
	Repository     string          `json:"repository"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	Locales        *Locales        `json:"locales"`
	Tags           []string        `json:"tags"`
	LogoURL        string          `json:"logo_url"`
	ScreenshotURLs []string        `json:"screenshot_urls"`
	Versions       *AppVersions    `json:"versions,omitempty"`
}

type Locales []string
type AppDescription map[string]string
type AppName map[string]string

func (l *Locales) UnmarshalJSON(data []byte) error {
	ss := make([]string, 0)
	if err := json.Unmarshal(data, &ss); err != nil {
		var m map[string]interface{}
		if err = json.Unmarshal(data, &m); err != nil {
			return err
		}
		for k := range m {
			ss = append(ss, k)
		}
	}
	(*l) = ss
	return nil
}

func (a *AppDescription) UnmarshalJSON(data []byte) error {
	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		var s string
		if err = json.Unmarshal(data, &s); err != nil {
			return err
		}
		m["en"] = s
	}
	(*a) = m
	return nil
}

func (a *AppName) UnmarshalJSON(data []byte) error {
	m := make(map[string]string)
	if err := json.Unmarshal(data, &m); err != nil {
		var s string
		if err = json.Unmarshal(data, &s); err != nil {
			return err
		}
		m["en"] = s
	}
	(*a) = m
	return nil
}

type AppVersions struct {
	Stable []string `json:"stable"`
	Beta   []string `json:"beta"`
	Dev    []string `json:"dev"`
}

type Developer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type VersionOptions struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	Sha256  string `json:"sha256"`
}

type Version struct {
	ID        string          `json:"_id,omitempty"`
	Rev       string          `json:"_rev,omitempty"`
	Slug      string          `json:"slug"`
	Editor    string          `json:"editor"`
	Type      string          `json:"type"`
	Version   string          `json:"version"`
	Manifest  json.RawMessage `json:"manifest"`
	CreatedAt time.Time       `json:"created_at"`
	URL       string          `json:"url"`
	Size      int64           `json:"size,string"`
	Sha256    string          `json:"sha256"`
	TarPrefix string          `json:"tar_prefix"`
}

func InitDBClient(addr, user, pass, prefix string) (*kivik.Client, error) {
	var err error

	var userInfo *url.Userinfo
	if user != "" {
		if pass != "" {
			userInfo = url.UserPassword(user, pass)
		} else {
			userInfo = url.User(user)
		}
	}

	u, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	u.User = userInfo

	client, err = kivik.New(ctx, "couch", u.String())
	if err != nil {
		return nil, err
	}
	clientURL = u

	if prefix != "" {
		AppsDB = prefix + "-" + AppsDB
		VersDB = prefix + "-" + VersDB
		EditorsDB = prefix + "-" + EditorsDB
	}

	dbs := []string{AppsDB, VersDB, EditorsDB}
	for _, dbName := range dbs {
		var ok bool
		ok, err = client.DBExists(ctx, dbName)
		if err != nil {
			return nil, err
		}
		if !ok {
			fmt.Printf("Creating database %s...", dbName)
			if err = client.CreateDB(ctx, dbName); err != nil {
				fmt.Println("failed")
				return nil, err
			}
			fmt.Println("ok")
		}
	}

	dbApps, err := client.DB(ctx, AppsDB)
	if err != nil {
		return nil, err
	}

	for name, index := range appsIndexes {
		err = dbApps.CreateIndex(ctx, "apps-index-"+name, "apps-index-"+name, index)
		if err != nil {
			return nil, err
		}
	}

	dbVers, err := client.DB(ctx, VersDB)
	if err != nil {
		return nil, err
	}

	err = dbVers.CreateIndex(ctx, "versions-index", "versions-index", versIndex)
	if err != nil {
		return nil, err
	}

	return client, err
}

func IsValidApp(app *App) error {
	var fields []string
	if app.Slug == "" || !validSlugReg.MatchString(app.Slug) {
		return ErrAppInvalid
	}
	if app.Editor == "" {
		fields = append(fields, "editor")
	}
	if !stringInArray(app.Type, validAppTypes) {
		fields = append(fields, "type")
	}
	if app.Repository != "" {
		if _, err := url.Parse(app.Repository); err != nil {
			fields = append(fields, "repository")
		}
	}
	if len(fields) > 0 {
		return errshttp.NewError(http.StatusBadRequest, "Invalid application, "+
			"the following fields are missing or erroneous: %s", strings.Join(fields, ", "))
	}
	return nil
}

func IsValidVersion(ver *VersionOptions) error {
	var fields []string
	if ver.Version == "" {
		fields = append(fields, "version")
	}
	if ver.URL == "" {
		fields = append(fields, "url")
	} else if _, err := url.Parse(ver.URL); err != nil {
		fields = append(fields, "url")
	}
	if h, err := hex.DecodeString(ver.Sha256); err != nil || len(h) != 32 {
		fields = append(fields, "sha256")
	}
	if len(fields) > 0 {
		return fmt.Errorf("Invalid version, "+
			"the following fields are missing or erroneous: %s", strings.Join(fields, ", "))
	}
	return nil
}

func CreateOrUpdateApp(app *App, editor *auth.Editor) (result *App, updated bool, err error) {
	if err = IsValidApp(app); err != nil {
		return
	}

	db, err := client.DB(ctx, AppsDB)
	if err != nil {
		return
	}
	oldApp, err := FindApp(app.Slug)
	if err != nil && err != ErrAppNotFound {
		return
	}
	now := time.Now().UTC()
	if err == ErrAppNotFound {
		app.ID = getAppID(app.Slug)
		app.Slug = app.ID
		app.Editor = editor.Name()
		app.CreatedAt = now
		app.UpdatedAt = now
		app.Versions = nil
		if app.Name == nil {
			v := make(AppName)
			app.Name = &v
		}
		if app.Description == nil {
			v := make(AppDescription)
			app.Description = &v
		}
		if app.Locales == nil {
			v := Locales(make([]string, 0))
			app.Locales = &v
		}
		if app.Tags == nil {
			app.Tags = make([]string, 0)
		}
		if app.ScreenshotURLs == nil {
			app.ScreenshotURLs = make([]string, 0)
		}
		_, _, err = db.CreateDoc(ctx, app)
		if err != nil {
			return
		}
		app.Versions = &AppVersions{
			Stable: make([]string, 0),
			Beta:   make([]string, 0),
			Dev:    make([]string, 0),
		}
		return app, true, nil
	}

	app.ID = oldApp.ID
	app.Rev = oldApp.Rev
	app.Slug = oldApp.Slug
	app.Type = oldApp.Type
	app.Editor = editor.Name()
	app.CreatedAt = oldApp.CreatedAt
	app.Versions = nil
	oldApp.Versions = nil
	oldApp.UpdatedAt = time.Time{}
	if app.Category == "" {
		app.Category = oldApp.Category
	}
	if app.Repository == "" {
		app.Repository = oldApp.Repository
	}
	if app.Name == nil {
		app.Name = oldApp.Name
	}
	if app.Developer == nil {
		app.Developer = oldApp.Developer
	}
	if app.Description == nil {
		app.Description = oldApp.Description
	}
	if app.Locales == nil {
		app.Locales = oldApp.Locales
	}
	if app.Tags == nil {
		app.Tags = oldApp.Tags
	}
	if app.ScreenshotURLs == nil {
		app.ScreenshotURLs = oldApp.ScreenshotURLs
	}
	if reflect.DeepEqual(app, oldApp) {
		return app, false, nil
	}
	app.UpdatedAt = now
	_, err = db.Put(ctx, app.ID, app)
	if err != nil {
		return
	}
	app.Versions, err = FindAppVersions(app.Slug)
	if err != nil {
		return
	}
	return app, true, nil
}

func DownloadVersion(opts *VersionOptions) (*Version, error) {
	return downloadVersion(opts)
}

func CreateVersion(ver *Version, app *App) error {
	_, err := FindVersion(ver.Slug, ver.Version)
	if err != ErrVersionNotFound {
		if err == nil {
			return ErrVersionAlreadyExists
		}
		return err
	}

	ver.Slug = app.Slug
	ver.Type = app.Type
	ver.Editor = app.Editor

	db, err := client.DB(ctx, VersDB)
	if err != nil {
		return err
	}
	_, _, err = db.CreateDoc(ctx, ver)
	return err
}

func downloadVersion(opts *VersionOptions) (ver *Version, err error) {
	url := opts.URL

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: %s", url, err)
		return
	}

	res, err := versionClient.Do(req)
	if err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: %s", url, err)
		return
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Could not reach version on specified url %s: server responded with code %d",
			url, res.StatusCode)
		return
	}

	h := sha256.New()
	var reader io.Reader
	counter := &Counter{}
	reader = io.LimitReader(res.Body, maxApplicationSize)
	reader = io.TeeReader(reader, counter)
	reader = io.TeeReader(reader, h)

	contentType := res.Header.Get("Content-Type")
	switch contentType {
	case
		"application/gzip",
		"application/x-gzip",
		"application/x-tgz",
		"application/tar+gzip":
		reader, err = gzip.NewReader(reader)
		if err != nil {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: %s", url, err)
			return
		}
	case "application/octet-stream":
		var r io.Reader
		if r, err = gzip.NewReader(reader); err == nil {
			reader = r
		}
	}

	var packVersion string
	var appType, prefix, editorName string
	var manifestContent []byte
	tarReader := tar.NewReader(reader)
	for {
		var hdr *tar.Header
		hdr, err = tarReader.Next()
		if err == io.EOF {
			break
		}
		if err == io.ErrUnexpectedEOF {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: file is too big %s", url, err)
			return
		}
		if err != nil {
			err = errshttp.NewError(http.StatusUnprocessableEntity,
				"Could not reach version on specified url %s: %s", url, err)
			return
		}

		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			continue
		}

		name := hdr.Name

		if split := strings.SplitN(name, "/", 2); len(split) == 2 {
			if prefix == "" {
				prefix = split[0]
			} else if prefix != split[0] {
				prefix = ""
			}
			name = split[1]
		}

		if name == "manifest.webapp" || name == "manifest.konnector" {
			if name == "manifest.webapp" {
				appType = "webapp"
			} else if name == "manifest.konnector" {
				appType = "konnector"
			}
			manifestContent, err = ioutil.ReadAll(tarReader)
			if err != nil {
				err = errshttp.NewError(http.StatusUnprocessableEntity,
					"Could not reach version on specified url %s: %s", url, err)
				return
			}
		}

		if name == "package.json" {
			var packageContent []byte
			packageContent, err = ioutil.ReadAll(tarReader)
			if err != nil {
				err = errshttp.NewError(http.StatusUnprocessableEntity,
					"Could not reach version on specified url %s: %s", url, err)
				return
			}
			var pack struct {
				Version string `json:"version"`
			}
			if err = json.Unmarshal(packageContent, &pack); err != nil {
				err = errshttp.NewError(http.StatusUnprocessableEntity,
					"File package.json is not valid in %s: %s", url, err)
				return
			}
			packVersion = pack.Version
		}
	}

	shasum, _ := hex.DecodeString(opts.Sha256)
	if !bytes.Equal(shasum, h.Sum(nil)) {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Checksum does not match the calculated one")
		return
	}

	if len(manifestContent) == 0 {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Application tarball does not contain a manifest")
		return
	}

	var manifest map[string]interface{}
	if err = json.Unmarshal(manifestContent, &manifest); err != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Content of the manifest is not JSON valid: %s", err)
		return
	}

	var errm error
	editorName, ok := manifest["editor"].(string)
	if !ok || editorName == "" {
		errm = multierror.Append(errm,
			fmt.Errorf("%q field is empty", "editor"))
	}

	slug, ok := manifest["slug"].(string)
	if !ok || slug == "" {
		errm = multierror.Append(errm,
			fmt.Errorf("%q field is empty", "slug"))
	}

	{
		version, ok := manifest["version"].(string)
		var match bool
		if !ok {
			// nothing
		} else if GetVersionChannel(opts.Version) != Dev {
			match = opts.Version == version
		} else {
			match = VersionMatch(opts.Version, version)
		}
		if !match {
			errm = multierror.Append(errm,
				fmt.Errorf("%q field does not match (%q != %q)",
					"version", version, opts.Version))
		}
		if packVersion != "" {
			if GetVersionChannel(opts.Version) != Dev {
				match = opts.Version != packVersion
			} else {
				match = VersionMatch(opts.Version, packVersion)
			}
			if !match {
				errm = multierror.Append(errm,
					fmt.Errorf("version from package.json (%q != %q)",
						version, packVersion))
			}
		}
	}
	if errm != nil {
		err = errshttp.NewError(http.StatusUnprocessableEntity,
			"Content of the manifest does not match: %s", errm)
		return
	}

	ver = new(Version)
	ver.ID = getVersionID(slug, opts.Version)
	ver.Slug = slug
	ver.Version = opts.Version
	ver.Type = appType
	ver.URL = opts.URL
	ver.Sha256 = opts.Sha256
	ver.Editor = editorName
	ver.Manifest = manifestContent
	ver.Size = counter.Written()
	ver.TarPrefix = prefix
	ver.CreatedAt = time.Now().UTC()
	return
}

func VersionMatch(ver1, ver2 string) bool {
	v1 := SplitVersion(ver1)
	v2 := SplitVersion(ver2)
	return v1[0] == v2[0] && v1[1] == v2[1] && v1[2] == v2[2]
}

func VersionLess(ver1, ver2 string) bool {
	v1 := SplitVersion(ver1)
	v2 := SplitVersion(ver2)
	if v1[0] < v2[0] {
		return true
	}
	if v1[0] == v2[0] && v1[1] < v2[1] {
		return true
	}
	if v1[0] == v2[0] && v1[1] == v2[1] && v1[2] < v2[2] {
		return true
	}
	return false
}

func GetVersionChannel(version string) Channel {
	if strings.Contains(version, devSuffix) {
		return Dev
	}
	if strings.Contains(version, betaSuffix) {
		return Beta
	}
	return Stable
}

func SplitVersion(version string) (v [3]string) {
	switch GetVersionChannel(version) {
	case Beta:
		version = version[:strings.Index(version, betaSuffix)]
	case Dev:
		version = version[:strings.Index(version, devSuffix)]
	}
	s := strings.SplitN(version, ".", 3)
	v[0] = s[0]
	v[1] = s[1]
	v[2] = s[2]
	return
}

func strToChannel(channel string) (Channel, error) {
	switch channel {
	case "stable":
		return Stable, nil
	case "beta":
		return Beta, nil
	case "dev":
		return Dev, nil
	default:
		return Stable, ErrChannelInvalid
	}
}

func channelToStr(channel Channel) string {
	switch channel {
	case Stable:
		return "stable"
	case Beta:
		return "beta"
	case Dev:
		return "dev"
	}
	panic("Unknown channel")
}
