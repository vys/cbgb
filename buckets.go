package cbgb

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io/ioutil"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
)

// Holder of buckets.
type Buckets struct {
	buckets  map[string]Bucket
	dir      string // Directory where all buckets are stored.
	lock     sync.Mutex
	settings *BucketSettings
}

// Build a new holder of buckets.
func NewBuckets(dirForBuckets string, settings *BucketSettings) (*Buckets, error) {
	if !isDir(dirForBuckets) {
		return nil, errors.New(fmt.Sprintf("not a directory: %v", dirForBuckets))
	}
	buckets := &Buckets{
		buckets:  map[string]Bucket{},
		dir:      dirForBuckets,
		settings: settings.Copy(),
	}
	return buckets, nil
}

// Create a new named bucket.
// Return the new bucket, or nil if the bucket already exists.
//
// TODO: Need clearer names around New vs Create vs Open vs Destroy,
// especially now that there's persistence.
func (b *Buckets) New(name string,
	defaultSettings *BucketSettings) (rv Bucket, err error) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if b.buckets[name] != nil {
		return nil, errors.New(fmt.Sprintf("bucket already exists: %v", name))
	}

	settings := &BucketSettings{}
	if defaultSettings != nil {
		settings = defaultSettings.Copy()
	}
	settings.UUID = CreateNewUUID()

	bdir, err := b.Path(name)
	if err != nil {
		return nil, err
	}

	if settings.MemoryOnly < MemoryOnly_LEVEL_PERSIST_NOTHING {
		// If an accessible bdir directory exists already, it's ok.
		if err = os.MkdirAll(bdir, 0777); err != nil && !isDir(bdir) {
			return nil, errors.New(fmt.Sprintf("could not access bucket dir: %v", bdir))
		}
	}

	_, err = settings.load(bdir)
	if err != nil {
		return nil, err
	}

	if rv, err = NewBucket(bdir, settings); err != nil {
		return nil, err
	}

	b.buckets[name] = rv
	return rv, nil
}

func (b *Buckets) GetNames() []string {
	b.lock.Lock()
	defer b.lock.Unlock()

	res := make([]string, 0, len(b.buckets))
	for name, _ := range b.buckets {
		res = append(res, name)
	}
	return res
}

// Get the named bucket (or nil if it doesn't exist).
func (b *Buckets) Get(name string) Bucket {
	b.lock.Lock()
	defer b.lock.Unlock()

	return b.buckets[name]
}

// Close the named bucket, optionally purging all its files.
func (b *Buckets) Close(name string, purgeFiles bool) {
	b.lock.Lock()
	defer b.lock.Unlock()

	if bucket := b.buckets[name]; bucket != nil {
		bucket.Close()
		delete(b.buckets, name)
	}

	if purgeFiles {
		bp, err := b.Path(name)
		if err == nil {
			os.RemoveAll(bp)
		}
	}
}

func (b *Buckets) CloseAll() {
	if b == nil {
		return
	}
	b.lock.Lock()
	defer b.lock.Unlock()

	for _, bucket := range b.buckets {
		bucket.Close()
	}
}

func (b *Buckets) Path(name string) (string, error) {
	bp, err := BucketPath(name)
	if err != nil {
		return "", err
	}
	return path.Join(b.dir, bp), nil
}

func BucketPath(bucketName string) (string, error) {
	match, err := regexp.MatchString("^[A-Za-z0-9\\-_]+$", bucketName)
	if err != nil {
		return "", err
	}
	if !match {
		return "", fmt.Errorf("bad bucket name: %v", bucketName)
	}
	c := uint16(crc32.ChecksumIEEE([]byte(bucketName)))
	lo := fmt.Sprintf("%02x", c&0xff)
	hi := fmt.Sprintf("%02x", c>>8)
	// Example result for "default" bucket: "$BUCKETS_DIR/00/df/default-bucket".
	return path.Join(hi, lo, bucketName+BUCKET_DIR_SUFFIX), nil
}

// Reads the buckets directory and returns list of bucket names.
func (b *Buckets) LoadNames() ([]string, error) {
	res := []string{}
	listHi, err := ioutil.ReadDir(b.dir)
	if err != nil {
		return nil, err
	}
	for _, entryHi := range listHi {
		if !entryHi.IsDir() {
			continue
		}
		pathHi := path.Join(b.dir, entryHi.Name())
		listLo, err := ioutil.ReadDir(pathHi)
		if err != nil {
			return nil, err
		}
		for _, entryLo := range listLo {
			if !entryLo.IsDir() {
				continue
			}
			pathLo := path.Join(pathHi, entryLo.Name())
			list, err := ioutil.ReadDir(pathLo)
			if err != nil {
				return nil, err
			}
			for _, entry := range list {
				if !entry.IsDir() ||
					!strings.HasSuffix(entry.Name(), BUCKET_DIR_SUFFIX) {
					continue
				}
				res = append(res,
					entry.Name()[0:len(entry.Name())-len(BUCKET_DIR_SUFFIX)])
			}
		}
	}
	return res, nil
}

// Loads all buckets from the buckets directory tree.  If
// errorIfBucketAlreadyExists is false any existing (already loaded)
// buckets are left unchanged (existing buckets are not reloaded).
func (b *Buckets) Load(ignoreIfBucketAlreadyExists bool) error {
	bucketNames, err := b.LoadNames()
	if err != nil {
		return err
	}
	for _, bucketName := range bucketNames {
		if b.Get(bucketName) != nil {
			if !ignoreIfBucketAlreadyExists {
				return fmt.Errorf("loading bucket %v, but it exists already",
					bucketName)
			}
			log.Printf("loading bucket: %v, already loaded", bucketName)
			continue
		}

		err = b.LoadBucket(bucketName)
		if err != nil {
			return err
		}
	}
	return nil
}

// Load a specific bucket by name.
func (b *Buckets) LoadBucket(name string) error {
	log.Printf("loading bucket: %v", name)
	bucket, err := b.New(name, b.settings)
	if err != nil {
		return err
	}
	return bucket.Load()
}
