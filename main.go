package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/CyCoreSystems/kubetemplate"
	"github.com/CyCoreSystems/netdiscover/discover"
	"github.com/pkg/errors"
)

const ariUsername = "k8s-asterisk-config"
const secretFilename = ".k8s-generated-secret"
const renderFlagFilename = ".asterisk-config"

var maxShortDeaths = 10
var minRuntime = time.Minute
var defaultMinReloadInterval = 5 * time.Second

// Service maintains an Asterisk configuration set
type Service struct {
	// Discoverer is the engine which should be used for network discovery
	Discoverer discover.Discoverer

	// Secret is the password which should be used for internal administrative authentication
	Secret string

	// SourceFile is the source zip file in the filesystem
	// This can be a mounted secret, or a just a zip file embeded in the container
	SourceFile string

	// SourceSecret indicates the source zip file is stored in a secret
	SourceSecret string

	// Namespace is the current namespace where to watch for secret changes
	Namespace string

	// CustomRoot is the directory which contains the tree of custom configuration templates
	CustomRoot string

	// DefaultsRoot is the directory which contains the default configuration templates
	DefaultsRoot string

	// ExportRoot is the destination directory to which the rendered configuration set will be exported.
	ExportRoot string

	// Modules is the list of Asterisk modules which should be reloaded after each render is complete.
	Modules string

	// engine is the template rendering and monitoring engine
	engine *kubetemplate.Engine

	// secretEngine is the engine used to react to SourceSecret changes
	secretEngine *kubetemplate.Engine
}

// nolint: gocyclo
func main() {
	var err error

	cloud := ""
	if os.Getenv("CLOUD") != "" {
		cloud = os.Getenv("CLOUD")
	}
	disc := getDiscoverer(cloud)

	sourceFile := "/source/asterisk-config.zip"
	if os.Getenv("SOURCE") != "" {
		sourceFile = os.Getenv("SOURCE")
	}

	secretSourceName := ""
	if os.Getenv("SECRET_SOURCE_NAME") != "" {
		secretSourceName = os.Getenv("SECRET_SOURCE_NAME")
		sourceFile = ""
	}

	namespace := ""
	if os.Getenv("POD_NAMESPACE") != "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}

	defaultsRoot := "/defaults"
	if os.Getenv("DEFAULTS_DIR") != "" {
		defaultsRoot = os.Getenv("DEFAULTS_DIR")
	}

	customRoot := "/custom"
	if os.Getenv("CUSTOM_DIR") != "" {
		customRoot = os.Getenv("CUSTOM_DIR")
	}
	if err := os.MkdirAll(customRoot, os.ModePerm); err != nil {
		log.Println("failed to ensure custom directory", customRoot, ":", err.Error())
		os.Exit(1)
	}

	exportRoot := "/etc/asterisk"
	if os.Getenv("EXPORT_DIR") != "" {
		exportRoot = os.Getenv("EXPORT_DIR")
	}
	if err = os.MkdirAll(exportRoot, os.ModePerm); err != nil {
		log.Println("failed to ensure destination directory", exportRoot, ":", err.Error())
		os.Exit(1)
	}

	modules := "res_pjsip.so"
	if os.Getenv("RELOAD_MODULES") != "" {
		modules = os.Getenv("RELOAD_MODULES")
	}

	secret := os.Getenv("ARI_AUTOSECRET")
	if secret == "" {
		secret, err = getOrCreateSecret(exportRoot)
		if err != nil {
			log.Println("failed to get secret:", err)
			os.Exit(1)
		}
		os.Setenv("ARI_AUTOSECRET", secret)
	}

	var shortDeaths int
	var t time.Time
	for shortDeaths < maxShortDeaths {

		svc := &Service{
			Discoverer:   disc,
			Secret:       secret,
			SourceFile:   sourceFile,
			SourceSecret: secretSourceName,
			Namespace:    namespace,
			CustomRoot:   customRoot,
			DefaultsRoot: defaultsRoot,
			ExportRoot:   exportRoot,
			Modules:      modules,
		}

		t = time.Now()
		log.Println("running service")
		err := svc.Run()
		log.Println("service exited:", err)
		if time.Since(t) < minRuntime {
			shortDeaths++
		} else {
			shortDeaths = 0
		}
	}

	log.Println("asterisk-config exiting")
	os.Exit(1)
}

// Run executes the Service
func (s *Service) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	renderChan := make(chan error, 1)

	r := newReloader(ctx, ariUsername, s.Secret, s.Modules)

	s.engine = kubetemplate.NewEngine(renderChan, s.Discoverer)
	defer s.engine.Close()

	s.secretEngine = kubetemplate.NewEngine(renderChan, s.Discoverer)
	defer s.secretEngine.Close()

	// Run the initial full render cycle
	if err := s.renderFull(); err != nil {
		return errors.Wrap(err, "failed to run the initial render")
	}

	r.Reload()

	s.engine.FirstRenderComplete(true)

	// Start watching the configuration secret using Kubetemplate
	if s.SourceSecret != "" {
		if _, err := s.secretEngine.SecretBinary(s.SourceSecret, s.Namespace, "asterisk-config.zip"); err != nil {
			return errors.Wrap(err, "failure during source secret watch")
		}
		s.secretEngine.FirstRenderComplete(true)
	}

	// Render loop
	for {
		if err := <-renderChan; err != nil {
			return errors.Wrap(err, "failure during watch")
		}
		log.Println("change detected")

		// Run the full render cycle to react to config change
		if err := s.renderFull(); err != nil {
			return errors.Wrap(err, "failed to re-render configuration")
		}

		// Reload the Asterik modules
		r.Reload()
	}
}

func clearDir(dir string) error {
	files, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		return err
	}
	for _, file := range files {
		err = os.RemoveAll(file)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) renderFull() error {
	if s.SourceSecret != "" {
		// Read the zip from the Secret
		if err := s.writeSecretFile(); err != nil {
			return errors.Wrap(err, "failed to extract secret")
		}

		s.SourceFile = "/asterisk-config.zip"
	}

	// Need to make sure that the ExportRoot is clean before rendering the
	// template because if we extract something to it, and later the secret
	// changes, the new zip might not have some files that it had before, and
	// we end up with inconsistent state. Removing everything does not affect
	// Asterisk as the files will only be considered when reloading modules.
	if err := clearDir(s.ExportRoot); err != nil {
		return errors.Wrap(err, "failed to cleanup export directory")
	}

	// Extract the source file
	if err := extractSource(s.SourceFile, s.CustomRoot); err != nil {
		return errors.Wrap(err, "failed extract source from")
	}

	// Export defaults
	if err := s.renderDefaults(); err != nil {
		return errors.Wrap(err, "failed to render defaults")
	}

	// Export custom configs
	if err := s.renderCustom(); err != nil {
		return errors.Wrap(err, "failed to render initial configuration")
	}

	// Write out render flag file to signal completion
	if err := ioutil.WriteFile(path.Join(s.ExportRoot, renderFlagFilename), []byte("complete"), 0666); err != nil {
		return errors.Wrap(err, "failed to write render flag file")
	}

	return nil
}

func (s *Service) writeSecretFile() error {
	data, err := s.secretEngine.SecretBinary(s.SourceSecret, s.Namespace, "asterisk-config.zip")
	if err != nil {
		return errors.Wrap(err, "failure during source secret fetch")
	}

	if err := ioutil.WriteFile("/asterisk-config.zip", data, 0666); err != nil {
		return errors.Wrap(err, "failed to write secret data")
	}

	return nil
}

func (s *Service) renderDefaults() error {
	return render(s.engine, s.DefaultsRoot, s.ExportRoot)
}

func (s *Service) renderCustom() error {
	return render(s.engine, s.CustomRoot, s.ExportRoot)
}

func getDiscoverer(cloud string) discover.Discoverer {
	switch cloud {
	case "aws":
		return discover.NewAWSDiscoverer()
	case "azure":
		return discover.NewAzureDiscoverer()
	case "digitalocean":
		return discover.NewDigitalOceanDiscoverer()
	case "do":
		return discover.NewDigitalOceanDiscoverer()
	case "gcp":
		return discover.NewGCPDiscoverer()
	case "":
		return discover.NewDiscoverer()
	default:
		log.Printf("WARNING: unhandled cloud %s\n", cloud)
		return discover.NewDiscoverer()
	}
}

func getOrCreateSecret(exportRoot string) (string, error) {
	secret := genSecret()
	secretPath := path.Join(exportRoot, secretFilename)

	// Determine if a secret has already been generated
	if data, err := ioutil.ReadFile(secretPath); err == nil {
		if len(data) > 0 {
			return string(data), nil
		}
	}

	if err := ioutil.WriteFile(secretPath, []byte(secret), 0600); err != nil {
		return "", errors.Wrap(err, "failed to write secret to file")
	}
	return secret, nil
}

func render(e *kubetemplate.Engine, customRoot string, exportRoot string) error {
	var fileCount int

	err := filepath.Walk(customRoot, func(fn string, info os.FileInfo, err error) error {
		if err != nil {
			return errors.Wrapf(err, "failed to access file %s", fn)
		}

		isTemplate := path.Ext(fn) == ".tmpl"

		outFile := path.Join(exportRoot, strings.TrimPrefix(fn, customRoot))
		if isTemplate {
			outFile = strings.TrimSuffix(outFile, ".tmpl")
		}

		if info.IsDir() {
			return os.MkdirAll(outFile, os.ModePerm)
		}
		if err = os.MkdirAll(path.Dir(outFile), os.ModePerm); err != nil {
			return errors.Wrapf(err, "failed to create destination directory %s", path.Dir(outFile))
		}
		fileCount++

		out, err := os.Create(outFile)
		if err != nil {
			return errors.Wrapf(err, "failed to open file for writing: %s", outFile)
		}
		defer out.Close() // nolint: errcheck

		in, err := os.Open(fn) // nolint: gosec
		if err != nil {
			return errors.Wrapf(err, "failed to open template for reading: %s", fn)
		}
		defer in.Close() // nolint: errcheck

		if isTemplate {
			return kubetemplate.Render(e, in, out)
		}

		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		return err
	}

	if fileCount < 1 {
		return errors.New("no files processed")
	}

	return nil
}

func waitAsterisk(username, secret string) error {
	r, err := http.NewRequest("GET", "http://127.0.0.1:8088/ari/asterisk/variable?variable=ASTERISK_CONFIG_SYSTEM_READY", nil)
	if err != nil {
		return errors.Wrap(err, "failed to construct ping request")
	}
	r.Header.Set("Content-Type", "application/json")
	r.SetBasicAuth(username, secret)

	type response struct {
		Value string `json:"value"`
	}
	resp := new(response)

	for {
		time.Sleep(time.Second / 2)

		ret, err := http.DefaultClient.Do(r)
		if err != nil {
			continue
		}

		if err = json.NewDecoder(ret.Body).Decode(resp); err != nil {
			// failed to decode into resp format
			log.Println("failed to decode Asterisk response:", err)
			continue
		}
		if resp.Value != "1" {
			// not yet ready
			continue
		}

		// System ready
		log.Println("Asterisk ready")
		return nil
	}
}

func extractSource(source, customRoot string) (err error) {
	if strings.HasPrefix(source, "http") {
		source, err = downloadSource(source)
		if err != nil {
			return errors.Wrap(err, "failed to download source")
		}
	}

	r, err := zip.OpenReader(source)
	if err != nil {
		return errors.Wrap(err, "failed to open source archive")
	}
	defer r.Close() // nolint: errcheck

	for _, f := range r.File {

		in, err := f.Open()
		if err != nil {
			return errors.Wrapf(err, "failed to read file %s", f.Name)
		}
		defer in.Close() // nolint: errcheck

		dest := path.Join(customRoot, f.Name)
		if f.FileInfo().IsDir() {
			if err = os.MkdirAll(dest, os.ModePerm); err != nil {
				return errors.Wrapf(err, "failed to create destination directory %s", f.Name)
			}
			continue
		}

		if err = os.MkdirAll(path.Dir(dest), os.ModePerm); err != nil {
			return errors.Wrapf(err, "failed to create destination directory %s", path.Dir(dest))
		}

		out, err := os.Create(dest)
		if err != nil {
			return errors.Wrapf(err, "failed to create file %s", dest)
		}

		_, err = io.Copy(out, in)
		out.Close() // nolint
		if err != nil {
			return errors.Wrapf(err, "error writing file %s", dest)
		}

	}

	return nil
}

func downloadSource(uri string) (string, error) {
	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to construct web request to %s", uri)
	}

	if os.Getenv("URL_USERNAME") != "" {
		req.SetBasicAuth(os.Getenv("URL_USERNAME"), os.Getenv("URL_PASSWORD"))
	}
	if os.Getenv("URL_AUTHORIZATION") != "" {
		req.Header.Add("Authorization", os.Getenv("URL_AUTHORIZATION"))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() // nolint: errcheck

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", errors.Errorf("request failed: %s", resp.Status)
	}
	if resp.ContentLength < 1 {
		return "", errors.New("empty response")
	}

	tf, err := ioutil.TempFile("", "config-download")
	if err != nil {
		return "", errors.Wrap(err, "failed to create temporary file for download")
	}
	defer tf.Close() // nolint: errcheck

	_, err = io.Copy(tf, resp.Body)

	return tf.Name(), err
}

type reloader struct {
	lastReload        time.Time
	minReloadInterval time.Duration

	username string
	secret   string

	modules []string

	needReload bool

	mu sync.Mutex
}

func newReloader(ctx context.Context, username, secret, modules string) *reloader {
	r := &reloader{
		minReloadInterval: defaultMinReloadInterval,
		username:          username,
		secret:            secret,
	}

	for _, m := range strings.Split(modules, ",") {
		r.modules = append(r.modules, strings.TrimSpace(m))
	}

	go r.run(ctx)

	return r
}

func (r *reloader) run(ctx context.Context) {
	// Wait for Asterisk to come up before proceeding, so as to not interrupt
	// normal Asterisk loading with a reload
	log.Println("Waiting for Asterisk to be ready...")
	if err := waitAsterisk(r.username, r.secret); err != nil {
		log.Fatalln("failed to wait for Asterisk to come up:", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.minReloadInterval):
		}

		if err := r.maybeRunReload(); err != nil {
			log.Println("failed to reload modules", err)
		}
	}
}

func (r *reloader) maybeRunReload() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.needReload {
		if err := r.reload(); err != nil {
			return err
		}

		r.needReload = false
	}

	return nil
}

func (r *reloader) Reload() {
	r.mu.Lock()
	r.needReload = true
	r.mu.Unlock()
}

func (r *reloader) reload() error {
	log.Println("reloading Asterisk modules")
	for _, m := range r.modules {
		if err := r.reloadModule(m); err != nil {
			return err
		}
	}
	log.Println("Asterisk modules reloaded")

	return nil
}

func (r *reloader) reloadModule(name string) error {
	url := fmt.Sprintf("http://127.0.0.1:8088/ari/asterisk/modules/%s", name)

	req, err := http.NewRequest("PUT", url, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to construct module reload request for module %s", name)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(r.username, r.secret)

	ret, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed to contact ARI to reload module %s", name)
	}
	ret.Body.Close() // nolint

	switch ret.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return errors.Errorf("module %s not already loaded", name)
	case http.StatusUnauthorized:
		return errors.Errorf("module %s failed to reload due bad authentication", name)
	case 409:
		return errors.Errorf("module %s could not be reloaded", name)
	default:
		return errors.Errorf("module %s reload failed: %s", name, ret.Status)
	}
}
