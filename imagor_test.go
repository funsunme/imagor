package imagor

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func jsonStr(v interface{}) string {
	buf, _ := json.Marshal(v)
	return string(buf)
}

func TestWithUnsafe(t *testing.T) {
	app := New(WithUnsafe(true))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/unsafe/foo.jpg", nil))
	assert.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))
}

func TestWithSecret(t *testing.T) {
	app := New(WithSecret("1234"))

	w := httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/_-19cQt1szHeUV0WyWFntvTImDI=/foo.jpg", nil))
	assert.Equal(t, 200, w.Code)

	w = httptest.NewRecorder()
	app.ServeHTTP(w, httptest.NewRequest(
		http.MethodGet, "https://example.com/foo.jpg", nil))
	assert.Equal(t, 403, w.Code)
	assert.Equal(t, w.Body.String(), jsonStr(ErrSignatureMismatch))
}

type mapStore struct {
	Map     map[string][]byte
	LoadCnt map[string]int
	SaveCnt map[string]int
}

func (s *mapStore) Load(r *http.Request, image string) ([]byte, error) {
	buf, ok := s.Map[image]
	if !ok {
		return nil, ErrNotFound
	}
	s.LoadCnt[image] = s.LoadCnt[image] + 1
	return buf, nil
}
func (s *mapStore) Save(ctx context.Context, image string, buf []byte) error {
	s.Map[image] = buf
	s.SaveCnt[image] = s.SaveCnt[image] + 1
	return nil
}

func TestWithLoadersStorages(t *testing.T) {
	store := &mapStore{
		Map: map[string][]byte{}, LoadCnt: map[string]int{}, SaveCnt: map[string]int{},
	}
	app := New(
		WithLoaders(
			store,
			LoaderFunc(func(r *http.Request, image string) ([]byte, error) {
				if image == "foo" {
					return []byte("bar"), nil
				}
				return nil, ErrPass
			}),
			LoaderFunc(func(r *http.Request, image string) ([]byte, error) {
				if image == "ping" {
					return []byte("pong"), nil
				}
				return nil, ErrPass
			}),
			LoaderFunc(func(r *http.Request, image string) ([]byte, error) {
				if image == "beep" {
					return []byte("boop"), nil
				}
				return nil, ErrPass
			}),
		),
		WithStorages(store),
		WithUnsafe(true),
	)
	t.Run("ok", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/foo", nil))
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "bar", w.Body.String())

		w = httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/ping", nil))
		assert.Equal(t, 200, w.Code)
		assert.Equal(t, "pong", w.Body.String())
	})
	t.Run("not found on pass", func(t *testing.T) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/boooo", nil))
		assert.Equal(t, 404, w.Code)
		assert.Equal(t, jsonStr(ErrNotFound), w.Body.String())
	})
	t.Run("should not save from same store", func(t *testing.T) {
		n := 5
		for i := 0; i < n; i++ {
			w := httptest.NewRecorder()
			app.ServeHTTP(w, httptest.NewRequest(
				http.MethodGet, "https://example.com/unsafe/beep", nil))
			assert.Equal(t, 200, w.Code)
			assert.Equal(t, "boop", w.Body.String())
		}
		assert.Equal(t, n-1, store.LoadCnt["beep"])
		assert.Equal(t, 1, store.SaveCnt["beep"])
	})
}
func TestSuppression(t *testing.T) {
	app := New(
		WithLoaders(
			LoaderFunc(func(r *http.Request, image string) (buf []byte, err error) {
				randBytes := make([]byte, 100)
				rand.Read(randBytes)
				time.Sleep(time.Millisecond * 100)
				return randBytes, nil
			}),
		),
		WithUnsafe(true),
	)
	n := 10
	type res struct {
		Name string
		Val  string
	}
	resChan := make(chan res)
	defer close(resChan)
	do := func(image string) {
		w := httptest.NewRecorder()
		app.ServeHTTP(w, httptest.NewRequest(
			http.MethodGet, "https://example.com/unsafe/"+image, nil))
		assert.Equal(t, 200, w.Code)
		resChan <- res{image, w.Body.String()}
	}
	for i := 0; i < n; i++ {
		// should suppress calls so every call of same image must be same value
		// though a and b must be different value
		go do("a")
		go do("b")
	}
	resMap := map[string]string{}
	for i := 0; i < n*2; i++ {
		res := <-resChan
		if val, ok := resMap[res.Name]; ok {
			assert.Equal(t, val, res.Val)
		} else {
			resMap[res.Name] = res.Val
		}
	}
	assert.NotEqual(t, resMap["a"], resMap["b"])
}
