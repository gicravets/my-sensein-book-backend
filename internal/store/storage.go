package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/studio-b12/gowebdav"
)

// Storage abstracts where book files + covers live. Keys are like "books/<id>.epub" or
// "covers/<id>". Drivers: local FS (default), S3-compatible, WebDAV. (ref: dev-plan-storage)
type Storage interface {
	Put(key string, data []byte) error
	Get(key string) ([]byte, error)
	Has(key string) bool
	Delete(key string) error
}

// ---------- local filesystem (default) ----------

type LocalStorage struct{ dir string }

func NewLocalStorage(dir string) *LocalStorage { return &LocalStorage{dir: dir} }

func (l *LocalStorage) path(key string) string { return filepath.Join(l.dir, filepath.FromSlash(key)) }

func (l *LocalStorage) Put(key string, data []byte) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}
func (l *LocalStorage) Get(key string) ([]byte, error) { return os.ReadFile(l.path(key)) }
func (l *LocalStorage) Has(key string) bool            { _, err := os.Stat(l.path(key)); return err == nil }
func (l *LocalStorage) Delete(key string) error        { return os.Remove(l.path(key)) }

// ---------- S3-compatible (AWS S3 / MinIO / Cloudflare R2) ----------

type S3Storage struct {
	client *minio.Client
	bucket string
}

func NewS3Storage(endpoint, bucket, region, accessKey, secret string, useSSL bool) (*S3Storage, error) {
	c, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secret, ""),
		Secure: useSSL,
		Region: region,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ok, _ := c.BucketExists(ctx, bucket); !ok {
		_ = c.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: region})
	}
	return &S3Storage{client: c, bucket: bucket}, nil
}

func (s *S3Storage) Put(key string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}
func (s *S3Storage) Get(key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}
func (s *S3Storage) Has(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	return err == nil
}
func (s *S3Storage) Delete(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}

// ---------- WebDAV (Nextcloud / self-host) ----------

type WebDAVStorage struct{ c *gowebdav.Client }

func NewWebDAVStorage(url, user, pass string) (*WebDAVStorage, error) {
	c := gowebdav.NewClient(url, user, pass)
	if err := c.Connect(); err != nil {
		return nil, err
	}
	return &WebDAVStorage{c: c}, nil
}

func (w *WebDAVStorage) Put(key string, data []byte) error {
	_ = w.c.MkdirAll(filepath.Dir(key), 0o755)
	return w.c.Write(key, data, 0o644)
}
func (w *WebDAVStorage) Get(key string) ([]byte, error) { return w.c.Read(key) }
func (w *WebDAVStorage) Has(key string) bool            { _, err := w.c.Stat(key); return err == nil }
func (w *WebDAVStorage) Delete(key string) error        { return w.c.Remove(key) }
