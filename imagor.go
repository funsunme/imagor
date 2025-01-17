package imagor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cshum/imagor/imagorpath"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Version = "0.9.12"

// Loader image loader interface
type Loader interface {
	Get(r *http.Request, key string) (*Blob, error)
}

// Storage image storage interface
type Storage interface {
	Get(r *http.Request, key string) (*Blob, error)
	Put(ctx context.Context, key string, blob *Blob) error
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (*Stat, error)
	Meta(ctx context.Context, key string) (*Meta, error)
}

// LoadFunc load function for Processor
type LoadFunc func(string) (*Blob, error)

// Processor process image buffer
type Processor interface {
	Startup(ctx context.Context) error
	Process(ctx context.Context, blob *Blob, p imagorpath.Params, load LoadFunc) (*Blob, error)
	Shutdown(ctx context.Context) error
}

// ResultKey generator
type ResultKey interface {
	Generate(p imagorpath.Params) string
}

// Imagor image resize HTTP handler
type Imagor struct {
	Unsafe                bool
	Signer                imagorpath.Signer
	BasePathRedirect      string
	Loaders               []Loader
	Storages              []Storage
	ResultStorages        []Storage
	Processors            []Processor
	RequestTimeout        time.Duration
	LoadTimeout           time.Duration
	SaveTimeout           time.Duration
	ProcessTimeout        time.Duration
	CacheHeaderTTL        time.Duration
	CacheHeaderSWR        time.Duration
	ProcessConcurrency    int64
	AutoWebP              bool
	AutoAVIF              bool
	ModifiedTimeCheck     bool
	DisableErrorBody      bool
	DisableParamsEndpoint bool
	BaseParams            string
	Logger                *zap.Logger
	Debug                 bool
	ResultKey             ResultKey

	g          singleflight.Group
	sema       *semaphore.Weighted
	baseParams imagorpath.Params
}

// New create new Imagor
func New(options ...Option) *Imagor {
	app := &Imagor{
		Logger:         zap.NewNop(),
		RequestTimeout: time.Second * 30,
		LoadTimeout:    time.Second * 20,
		SaveTimeout:    time.Second * 20,
		ProcessTimeout: time.Second * 20,
		CacheHeaderTTL: time.Hour * 24 * 7,
		CacheHeaderSWR: time.Hour * 24,
	}
	for _, option := range options {
		option(app)
	}
	if app.ProcessConcurrency > 0 {
		app.sema = semaphore.NewWeighted(app.ProcessConcurrency)
	}
	if app.Debug {
		app.debugLog()
	}
	if app.Signer == nil {
		app.Signer = imagorpath.NewDefaultSigner("")
	}
	app.BaseParams = strings.TrimSpace(app.BaseParams)
	if app.BaseParams != "" {
		app.BaseParams = strings.TrimSuffix(app.BaseParams, "/") + "/"
	}
	return app
}

// Startup Imagor startup lifecycle
func (app *Imagor) Startup(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Startup(ctx); err != nil {
			return
		}
	}
	return
}

// Shutdown Imagor shutdown lifecycle
func (app *Imagor) Shutdown(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Shutdown(ctx); err != nil {
			return
		}
	}
	return
}

// ServeHTTP implements http.Handler for Imagor operations
func (app *Imagor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.EscapedPath()
	if path == "/" || path == "" {
		if app.BasePathRedirect == "" {
			writeJSON(w, r, json.RawMessage(fmt.Sprintf(
				`{"imagor":{"version":"%s"}}`, Version,
			)))
		} else {
			http.Redirect(w, r, app.BasePathRedirect, http.StatusTemporaryRedirect)
		}
		return
	}
	p := imagorpath.Parse(path)
	if p.Params {
		if !app.DisableParamsEndpoint {
			writeJSONIndent(w, r, p)
		}
		return
	}
	blob, err := checkBlob(app.Do(r, p))
	if err == nil && p.Meta && blob != nil && blob.Meta != nil {
		writeJSON(w, r, blob.Meta)
		return
	}
	if !isBlobEmpty(blob) {
		w.Header().Set("Content-Type", blob.ContentType())
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			w.WriteHeader(499)
			return
		}
		e := WrapError(err)
		if app.DisableErrorBody {
			w.WriteHeader(e.Code)
			return
		}
		if !isBlobEmpty(blob) {
			reader, size, _ := blob.NewReader()
			if reader != nil {
				w.WriteHeader(e.Code)
				writeBody(w, r, reader, size)
				return
			}
		}
		w.WriteHeader(e.Code)
		writeJSON(w, r, e)
		return
	}
	if isBlobEmpty(blob) {
		return
	}
	reader, size, _ := blob.NewReader()
	setCacheHeaders(w, app.CacheHeaderTTL, app.CacheHeaderSWR)
	writeBody(w, r, reader, size)
	return
}

// Do executes Imagor operations
func (app *Imagor) Do(r *http.Request, p imagorpath.Params) (blob *Blob, err error) {
	var ctx = WithDefer(r.Context())
	var cancel func()
	if app.RequestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.RequestTimeout)
		Defer(ctx, cancel)
		r = r.WithContext(ctx)
	}
	if !(app.Unsafe && p.Unsafe) && app.Signer != nil && app.Signer.Sign(p.Path) != p.Hash {
		err = ErrSignatureMismatch
		if app.Debug {
			app.Logger.Debug("sign-mismatch", zap.Any("params", p), zap.String("expected", app.Signer.Sign(p.Path)))
		}
		return
	}
	if app.BaseParams != "" {
		p = imagorpath.Apply(p, app.BaseParams)
		p.Path = imagorpath.GeneratePath(p)
	}
	// auto WebP / AVIF
	if app.AutoWebP || app.AutoAVIF {
		var hasFormat bool
		for _, f := range p.Filters {
			if f.Name == "format" {
				hasFormat = true
			}
		}
		if !hasFormat {
			accept := r.Header.Get("Accept")
			if app.AutoAVIF && strings.Contains(accept, "image/avif") {
				p.Filters = append(p.Filters, imagorpath.Filter{
					Name: "format",
					Args: "avif",
				})
				p.Path = imagorpath.GeneratePath(p)
			} else if app.AutoWebP && strings.Contains(accept, "image/webp") {
				p.Filters = append(p.Filters, imagorpath.Filter{
					Name: "format",
					Args: "webp",
				})
				p.Path = imagorpath.GeneratePath(p)
			}
		}
	}
	var resultKey string
	if app.ResultKey != nil {
		resultKey = app.ResultKey.Generate(p)
	} else {
		resultKey = strings.TrimPrefix(p.Path, "meta/")
	}
	load := func(image string) (*Blob, error) {
		b, _, err := app.loadStorage(r, image)
		return b, err
	}
	if p.Meta {
		if blob := app.loadResult(r, resultKey, p.Image, true); blob != nil {
			return blob, nil
		}
	}
	return app.suppress(ctx, "res:"+resultKey, func(ctx context.Context) (*Blob, error) {
		if !p.Meta {
			if blob := app.loadResult(r, resultKey, p.Image, false); blob != nil {
				return blob, nil
			}
		}
		if app.sema != nil {
			if err = app.sema.Acquire(ctx, 1); err != nil {
				app.Logger.Debug("acquire", zap.Error(err))
				return blob, err
			}
			defer app.sema.Release(1)
		}
		var isSave bool
		if blob, isSave, err = app.loadStorage(r, p.Image); err != nil {
			app.Logger.Debug("load", zap.Any("params", p), zap.Error(err))
			return blob, err
		}
		if isBlobEmpty(blob) {
			return blob, err
		}
		var cancel func()
		if app.ProcessTimeout > 0 {
			ctx, cancel = context.WithTimeout(ctx, app.ProcessTimeout)
			Defer(ctx, cancel)
		}
		for _, processor := range app.Processors {
			b, e := checkBlob(processor.Process(ctx, blob, p, load))
			if e == nil {
				blob = b
				err = nil
				if app.Debug {
					app.Logger.Debug("processed", zap.Any("params", p), zap.Any("meta", b.Meta))
				}
				break
			} else {
				if e == ErrPass {
					if !isBlobEmpty(b) {
						// pass to next processor
						blob = b
					}
					if app.Debug {
						app.Logger.Debug("process", zap.Any("params", p), zap.Error(e))
					}
				} else {
					err = e
					app.Logger.Warn("process", zap.Any("params", p), zap.Error(err))
					if errors.Is(err, context.DeadlineExceeded) {
						break
					}
				}
			}
		}
		if err == nil && len(app.ResultStorages) > 0 {
			app.save(ctx, app.ResultStorages, resultKey, blob)
		}
		if err != nil && isSave {
			app.del(ctx, app.Storages, p.Image)
		}
		return blob, err
	})
}

func (app *Imagor) loadStorage(r *http.Request, key string) (*Blob, bool, error) {
	var isSave bool
	b, err := app.suppress(r.Context(), "img:"+key, func(ctx context.Context) (blob *Blob, err error) {
		r = r.WithContext(ctx)
		var origin Storage
		blob, origin, err = app.load(r, app.Storages, app.Loaders, key, false)
		if err == nil && !isBlobEmpty(blob) && origin == nil && len(app.Storages) > 0 {
			isSave = true
			app.save(ctx, app.Storages, key, blob)
		}
		return
	})
	return b, isSave, err
}

func (app *Imagor) loadResult(r *http.Request, resultKey, imageKey string, metaMode bool) *Blob {
	ctx := r.Context()
	blob, origin, err := app.load(r, app.ResultStorages, nil, resultKey, metaMode)
	if err == nil && (!isBlobEmpty(blob) || metaMode) {
		if app.ModifiedTimeCheck && origin != nil {
			if resStat, err1 := origin.Stat(ctx, resultKey); resStat != nil && err1 == nil {
				if sourceStat, err2 := app.storageStat(ctx, imageKey); sourceStat != nil && err2 == nil {
					if !resStat.ModifiedTime.Before(sourceStat.ModifiedTime) {
						return blob
					}
				}
			}
		} else {
			return blob
		}
	}
	return nil
}

func (app *Imagor) load(
	r *http.Request, storages []Storage, loaders []Loader, key string, metaMode bool,
) (blob *Blob, origin Storage, err error) {
	if key == "" {
		err = ErrNotFound
		return
	}
	var ctx = r.Context()
	var cancel func()
	if app.LoadTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.LoadTimeout)
		Defer(ctx, cancel)
		r = r.WithContext(ctx)
	}
	if metaMode {
		for _, storage := range storages {
			m, e := storage.Meta(ctx, key)
			if e == nil && m != nil {
				blob = NewEmptyBlob()
				blob.Meta = m
				origin = storage
				return
			}
			err = e
		}
	} else {
		for _, storage := range storages {
			b, e := checkBlob(storage.Get(r, key))
			if !isBlobEmpty(b) {
				blob = b
				if e == nil {
					err = nil
					origin = storage
					return
				}
			}
			err = e
		}
		for _, loader := range loaders {
			b, e := checkBlob(loader.Get(r, key))
			if !isBlobEmpty(b) {
				blob = b
				if e == nil {
					err = nil
					return
				}
			}
			err = e
		}
	}
	if err == nil && isBlobEmpty(blob) && !metaMode {
		err = ErrNotFound
	}
	if app.Debug {
		if err == nil {
			app.Logger.Debug("loaded", zap.String("key", key))
		} else {
			app.Logger.Debug("load", zap.String("key", key), zap.Error(err))
		}
	}
	return
}

func (app *Imagor) storageStat(ctx context.Context, key string) (stat *Stat, err error) {
	for _, storage := range app.Storages {
		if stat, err = storage.Stat(ctx, key); stat != nil && err == nil {
			return
		}
	}
	return
}

func (app *Imagor) save(ctx context.Context, storages []Storage, key string, blob *Blob) {
	var cancel func()
	if app.SaveTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.SaveTimeout)
	}
	Defer(ctx, cancel)
	var wg sync.WaitGroup
	for _, storage := range storages {
		wg.Add(1)
		go func(storage Storage) {
			defer wg.Done()
			if err := storage.Put(ctx, key, blob); err != nil {
				app.Logger.Warn("save", zap.String("key", key), zap.Error(err))
			} else if app.Debug {
				app.Logger.Debug("saved", zap.String("key", key))
			}
		}(storage)
	}
	wg.Wait()
	return
}

func (app *Imagor) del(ctx context.Context, storages []Storage, key string) {
	var wg sync.WaitGroup
	for _, storage := range storages {
		wg.Add(1)
		go func(storage Storage) {
			defer wg.Done()
			if err := storage.Delete(ctx, key); err != nil {
				app.Logger.Warn("delete", zap.String("key", key), zap.Error(err))
			} else if app.Debug {
				app.Logger.Debug("deleted", zap.String("key", key))
			}
		}(storage)
	}
	wg.Wait()
	return
}

type suppressKey struct {
	Key string
}

func (app *Imagor) suppress(
	ctx context.Context,
	key string, fn func(ctx context.Context) (*Blob, error),
) (blob *Blob, err error) {
	if app.Debug {
		app.Logger.Debug("suppress", zap.String("key", key))
	}
	if isAcquired, ok := ctx.Value(suppressKey{key}).(bool); ok && isAcquired {
		// resolve deadlock
		return fn(ctx)
	}
	isCanceled := false
	ch := app.g.DoChan(key, func() (v interface{}, err error) {
		v, err = fn(context.WithValue(ctx, suppressKey{key}, true))
		if errors.Is(err, context.Canceled) {
			app.g.Forget(key)
			isCanceled = true
		}
		return v, err
	})
	select {
	case res := <-ch:
		if !isCanceled && errors.Is(res.Err, context.Canceled) {
			// resolve canceled
			return app.suppress(ctx, key, fn)
		}
		if res.Val != nil {
			return res.Val.(*Blob), res.Err
		}
		return nil, res.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (app *Imagor) debugLog() {
	if !app.Debug {
		return
	}
	var loaders, storages, resultStorages, processors []string
	for _, v := range app.Loaders {
		loaders = append(loaders, getType(v))
	}
	for _, v := range app.Storages {
		storages = append(storages, getType(v))
	}
	for _, v := range app.Processors {
		processors = append(processors, getType(v))
	}
	for _, v := range app.ResultStorages {
		resultStorages = append(resultStorages, getType(v))
	}
	app.Logger.Debug("imagor",
		zap.String("version", Version),
		zap.Bool("unsafe", app.Unsafe),
		zap.Duration("request_timeout", app.RequestTimeout),
		zap.Duration("load_timeout", app.LoadTimeout),
		zap.Duration("process_timeout", app.ProcessTimeout),
		zap.Duration("save_timeout", app.SaveTimeout),
		zap.Int64("process_concurrency", app.ProcessConcurrency),
		zap.Duration("cache_header_ttl", app.CacheHeaderTTL),
		zap.Strings("loaders", loaders),
		zap.Strings("storages", storages),
		zap.Strings("result_storages", resultStorages),
		zap.Strings("processors", processors),
	)
}

func setCacheHeaders(w http.ResponseWriter, ttl, swr time.Duration) {
	expires := time.Now().Add(ttl)

	w.Header().Add("Expires", strings.Replace(expires.Format(time.RFC1123), "UTC", "GMT", -1))
	w.Header().Add("Cache-Control", getCacheControl(ttl, swr))
}

func getCacheControl(ttl, swr time.Duration) string {
	if ttl == 0 {
		return "private, no-cache, no-store, must-revalidate"
	}
	var ttlSec = int64(ttl.Seconds())
	var val = fmt.Sprintf("public, s-maxage=%d, max-age=%d, no-transform", ttlSec, ttlSec)
	if swr > 0 && swr < ttl {
		val += fmt.Sprintf(", stale-while-revalidate=%d", int64(swr.Seconds()))
	}
	return val
}

func writeJSON(w http.ResponseWriter, r *http.Request, v interface{}) {
	buf, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf)
	}
	return
}

func writeJSONIndent(w http.ResponseWriter, r *http.Request, v interface{}) {
	buf, _ := json.MarshalIndent(v, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf)
	}
	return
}

func writeBody(w http.ResponseWriter, r *http.Request, reader io.ReadCloser, size int64) {
	defer func() {
		_ = reader.Close()
	}()
	if size > 0 {
		// total size known, use io.Copy
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, reader)
		}
	} else {
		// total size unknown, read all
		buf, _ := io.ReadAll(reader)
		w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(buf)
		}
	}
}

func getType(v interface{}) string {
	if t := reflect.TypeOf(v); t.Kind() == reflect.Ptr {
		return t.Elem().Name()
	} else {
		return t.Name()
	}
}
