package imagor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cshum/imagor/imagorpath"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

type LoadFunc func(string) (*File, error)

type Meta struct {
	Format      string `json:"format"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Orientation int    `json:"orientation"`
}

// Loader Load image from image source
type Loader interface {
	Load(r *http.Request, image string) (*File, error)
}

// Storage save image buffer
type Storage interface {
	Save(ctx context.Context, image string, file *File) error
}

// Store both a Loader and Storage
type Store interface {
	Loader
	Storage
}

// Processor process image buffer
type Processor interface {
	Startup(ctx context.Context) error
	Process(ctx context.Context, file *File, p imagorpath.Params, load LoadFunc) (*File, *Meta, error)
	Shutdown(ctx context.Context) error
}

// Imagor image resize HTTP handler
type Imagor struct {
	Version        string
	Unsafe         bool
	Secret         string
	Loaders        []Loader
	Storages       []Storage
	Processors     []Processor
	RequestTimeout time.Duration
	LoadTimeout    time.Duration
	SaveTimeout    time.Duration
	CacheHeaderTTL time.Duration
	Logger         *zap.Logger
	Debug          bool

	g singleflight.Group
}

// New create new Imagor
func New(options ...Option) *Imagor {
	app := &Imagor{
		Version:        "dev",
		Logger:         zap.NewNop(),
		RequestTimeout: time.Second * 30,
		LoadTimeout:    time.Second * 20,
		SaveTimeout:    time.Second * 20,
		CacheHeaderTTL: time.Hour * 24,
	}
	for _, option := range options {
		option(app)
	}
	if app.LoadTimeout > app.RequestTimeout || app.LoadTimeout == 0 {
		app.LoadTimeout = app.RequestTimeout
	}
	if app.Debug {
		app.debugLog()
	}
	return app
}

func (app *Imagor) Startup(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Startup(ctx); err != nil {
			return
		}
	}
	return
}

func (app *Imagor) Shutdown(ctx context.Context) (err error) {
	for _, processor := range app.Processors {
		if err = processor.Shutdown(ctx); err != nil {
			return
		}
	}
	return
}

func (app *Imagor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if path == "/" {
		resJSON(w, json.RawMessage(fmt.Sprintf(
			`{"imagor":{"version":"%s"}}`, app.Version,
		)))
		return
	}
	p := imagorpath.Parse(path)
	if p.Params {
		resJSONIndent(w, p)
		return
	}
	file, meta, err := app.Do(r, p)
	var buf []byte
	var ln int
	if !IsFileEmpty(file) {
		buf, _ = file.Bytes()
		ln = len(buf)
	}
	if meta != nil {
		if p.Meta {
			resJSON(w, meta)
			return
		} else {
			w.Header().Set("Content-Type", meta.ContentType)
		}
	} else if ln > 0 {
		w.Header().Set("Content-Type", http.DetectContentType(buf))
	}
	if err != nil {
		if e, ok := WrapError(err).(Error); ok {
			if e == ErrPass {
				// passed till the end means not found
				e = ErrNotFound
			}
			w.WriteHeader(e.Code)
			if ln > 0 {
				w.Header().Set("Content-Length", strconv.Itoa(ln))
				_, _ = w.Write(buf)
				return
			}
			resJSON(w, e)
		} else {
			resJSON(w, ErrInternal)
		}
		return
	}
	setCacheHeaders(w, app.CacheHeaderTTL)
	w.Header().Set("Content-Length", strconv.Itoa(ln))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf)
	return
}

func (app *Imagor) Do(r *http.Request, p imagorpath.Params) (file *File, meta *Meta, err error) {
	var cancel func()
	ctx := r.Context()
	if app.RequestTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
	}
	if !(app.Unsafe && p.Unsafe) && imagorpath.Sign(p.Path, app.Secret) != p.Hash {
		err = ErrSignatureMismatch
		if app.Debug {
			app.Logger.Debug("sign-mismatch", zap.Any("params", p), zap.String("expected", imagorpath.Sign(p.Path, app.Secret)))
		}
		return
	}
	if file, err = app.load(r, p.Image); err != nil {
		app.Logger.Debug("load", zap.Any("params", p), zap.Error(err))
		return
	}
	load := func(image string) (*File, error) {
		return app.load(r, image)
	}
	if IsFileEmpty(file) {
		return
	}
	for _, processor := range app.Processors {
		f, m, e := processor.Process(ctx, file, p, load)
		if e == nil {
			file = f
			meta = m
			err = nil
			if app.Debug {
				app.Logger.Debug("processed", zap.Any("params", p), zap.Any("meta", meta))
			}
			break
		} else {
			if e == ErrPass {
				if !IsFileEmpty(f) {
					// pass to next processor
					file = f
				}
				if app.Debug {
					app.Logger.Debug("process", zap.Any("params", p), zap.Error(e))
				}
			} else {
				err = e
				app.Logger.Warn("process", zap.Any("params", p), zap.Error(e))
			}
		}
	}
	return
}

func (app *Imagor) load(r *http.Request, image string) (*File, error) {
	var ctx = r.Context()
	var loadCtx = ctx
	var loadReq = r
	var cancel func()
	if app.LoadTimeout > 0 {
		loadCtx, cancel = context.WithTimeout(loadCtx, app.LoadTimeout)
		defer cancel()
		loadReq = r.WithContext(loadCtx)
	}
	return app.suppress(image, func() (file *File, err error) {
		var fromStore Store
		for _, loader := range app.Loaders {
			f, e := loader.Load(loadReq, image)
			if !IsFileEmpty(f) {
				file = f
			}
			if e == nil {
				err = nil
				fromStore, _ = loader.(Store)
				break
			}
			// should not log expected error as of now, as it has not reached the end
			if e != nil && e != ErrPass && e != ErrNotFound && !errors.Is(e, context.Canceled) {
				app.Logger.Warn("load", zap.String("image", image), zap.Error(e))
			} else if app.Debug {
				app.Logger.Debug("load", zap.String("image", image), zap.Error(e))
			}
			err = e
		}
		if err == nil {
			if app.Debug {
				app.Logger.Debug("loaded", zap.String("image", image))
			}
			if IsFileEmpty(file) {
				return
			}
			if len(app.Storages) > 0 {
				app.save(ctx, fromStore, app.Storages, image, file)
			}
		} else if !errors.Is(err, context.Canceled) {
			if err == ErrPass {
				err = ErrNotFound
			}
			// log non user-initiated error finally
			app.Logger.Warn("load", zap.String("image", image), zap.Error(err))
		}
		return
	})
}

func (app *Imagor) suppress(key string, fn func() (*File, error)) (file *File, err error) {
	v, err, _ := app.g.Do(key, func() (interface{}, error) {
		return fn()
	})
	if v != nil {
		return v.(*File), err
	}
	return nil, err
}

func (app *Imagor) save(
	ctx context.Context, from Storage, storages []Storage, image string, file *File,
) {
	var cancel func()
	if app.SaveTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, app.SaveTimeout)
	}
	defer cancel()
	var wg sync.WaitGroup
	for _, storage := range storages {
		if storage == from {
			// loaded from the same store, no need save again
			if app.Debug {
				app.Logger.Debug("skip-save", zap.String("image", image))
			}
			continue
		}
		wg.Add(1)
		go func(storage Storage) {
			defer wg.Done()
			if err := storage.Save(ctx, image, file); err != nil {
				app.Logger.Warn("save", zap.String("image", image), zap.Error(err))
			} else if app.Debug {
				app.Logger.Debug("saved", zap.String("image", image))
			}
		}(storage)
	}
	wg.Wait()
	return
}

func (app *Imagor) debugLog() {
	if !app.Debug {
		return
	}
	var loaders, storages, processors []string
	for _, v := range app.Loaders {
		loaders = append(loaders, getType(v))
	}
	for _, v := range app.Storages {
		storages = append(storages, getType(v))
	}
	for _, v := range app.Processors {
		processors = append(processors, getType(v))
	}
	app.Logger.Debug("imagor",
		zap.Bool("unsafe", app.Unsafe),
		zap.Duration("request_timeout", app.RequestTimeout),
		zap.Duration("load_timeout", app.LoadTimeout),
		zap.Duration("save_timeout", app.SaveTimeout),
		zap.Duration("cache_header_ttl", app.CacheHeaderTTL),
		zap.Strings("loaders", loaders),
		zap.Strings("storages", storages),
		zap.Strings("processors", processors),
	)
}

func setCacheHeaders(w http.ResponseWriter, ttl time.Duration) {
	expires := time.Now().Add(ttl)

	w.Header().Add("Expires", strings.Replace(expires.Format(time.RFC1123), "UTC", "GMT", -1))
	w.Header().Add("Cache-Control", getCacheControl(ttl))
}

func getCacheControl(ttl time.Duration) string {
	if ttl == 0 {
		return "private, no-cache, no-store, must-revalidate"
	}
	ttlSec := int(ttl.Seconds())
	return fmt.Sprintf("public, s-maxage=%d, max-age=%d, no-transform", ttlSec, ttlSec)
}

func resJSON(w http.ResponseWriter, v interface{}) {
	buf, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
	return
}

func resJSONIndent(w http.ResponseWriter, v interface{}) {
	buf, _ := json.MarshalIndent(v, "", "  ")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
	return
}

func getType(v interface{}) string {
	if t := reflect.TypeOf(v); t.Kind() == reflect.Ptr {
		return t.Elem().Name()
	} else {
		return t.Name()
	}
}
