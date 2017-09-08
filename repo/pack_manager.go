package repo

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kopia/kopia/blob"
)

const packObjectPrefix = "P"

type packInfo struct {
	currentPackData  bytes.Buffer
	currentPackIndex *packIndex
	currentPackID    string
}

type blockLocation struct {
	packIndex   int
	objectIndex int
}

type packManager struct {
	objectManager *ObjectManager
	storage       blob.Storage

	mu           sync.RWMutex
	blockToIndex map[string]*packIndex

	pendingPackIndexes packIndexes
	packGroups         map[string]*packInfo
}

func (p *packManager) enabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pendingPackIndexes != nil
}

func (p *packManager) blockIDToPackSection(blockID string) (ObjectIDSection, bool, error) {
	if strings.HasPrefix(blockID, packObjectPrefix) {
		return ObjectIDSection{}, false, nil
	}

	pi, err := p.ensurePackIndexesLoaded()
	if err != nil {
		return ObjectIDSection{}, false, fmt.Errorf("can't load pack index: %v", err)
	}

	ndx := pi[blockID]
	if ndx == nil {
		return ObjectIDSection{}, false, nil
	}

	blk := ndx.Items[blockID]
	if blk == "" {
		return ObjectIDSection{}, false, nil
	}

	if plus := strings.IndexByte(blk, '+'); plus > 0 {
		if start, err := strconv.ParseInt(blk[0:plus], 10, 64); err == nil {
			if length, err := strconv.ParseInt(blk[plus+1:], 10, 64); err == nil {
				if base, err := ParseObjectID(ndx.PackObject); err == nil {
					return ObjectIDSection{
						Base:   base,
						Start:  start,
						Length: length,
					}, true, nil
				}
			}
		}
	}

	return ObjectIDSection{}, false, fmt.Errorf("invalid pack index for %q", blockID)
}

func (p *packManager) begin() error {
	p.ensurePackIndexesLoaded()
	p.pendingPackIndexes = make(packIndexes)
	return nil
}

func (p *packManager) AddToPack(packGroup string, blockID string, data []byte) (ObjectID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// See if we already have this block ID in some pack.
	if _, ok := p.blockToIndex[blockID]; ok {
		return ObjectID{StorageBlock: blockID}, nil
	}

	g := p.packGroups[packGroup]
	if g == nil {
		g = &packInfo{}
		p.packGroups[packGroup] = g
	}

	if g.currentPackIndex == nil {
		g.currentPackIndex = &packIndex{
			Items:      make(map[string]string),
			PackGroup:  packGroup,
			CreateTime: time.Now().UTC(),
		}
		g.currentPackID = p.newPackID()
		p.pendingPackIndexes[g.currentPackID] = g.currentPackIndex
		g.currentPackData.Reset()
	}

	offset := g.currentPackData.Len()
	g.currentPackData.Write(data)
	g.currentPackIndex.Items[blockID] = fmt.Sprintf("%v+%v", int64(offset), int64(len(data)))

	if g.currentPackData.Len() >= p.objectManager.format.MaxPackFileLength {
		if err := p.finishCurrentPackLocked(); err != nil {
			return NullObjectID, err
		}
	}

	p.blockToIndex[blockID] = g.currentPackIndex
	return ObjectID{StorageBlock: blockID}, nil
}

func (p *packManager) finishPacking() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if err := p.finishCurrentPackLocked(); err != nil {
		return err
	}

	if err := p.savePackIndexes(); err != nil {
		return err
	}

	p.pendingPackIndexes = nil
	return nil
}

func (p *packManager) savePackIndexes() error {
	if len(p.pendingPackIndexes) == 0 {
		return nil
	}

	var jb bytes.Buffer
	if err := json.NewEncoder(&jb).Encode(p.pendingPackIndexes); err != nil {
		return fmt.Errorf("can't encode pack index: %v", err)
	}

	w := p.objectManager.NewWriter(WriterOptions{
		disablePacking:  true,
		BlockNamePrefix: packObjectPrefix,
		splitter:        newNeverSplitter(),
	})

	w.Write(jb.Bytes())
	if _, err := w.Result(); err != nil {
		return fmt.Errorf("can't save pack index object: %v", err)
	}

	return nil
}

func (p *packManager) finishCurrentPackLocked() error {
	for _, g := range p.packGroups {
		if err := p.finishPackLocked(g); err != nil {
			return err
		}
	}

	return nil
}

func (p *packManager) finishPackLocked(g *packInfo) error {
	if g.currentPackIndex == nil {
		return nil
	}
	w := p.objectManager.NewWriter(WriterOptions{
		Description:    fmt.Sprintf("pack:%v", g.currentPackID),
		splitter:       newNeverSplitter(),
		disablePacking: true,
	})
	defer w.Close()

	if _, err := g.currentPackData.WriteTo(w); err != nil {
		return fmt.Errorf("unable to write pack: %v", err)
	}
	g.currentPackData.Reset()
	oid, err := w.Result()

	if err != nil {
		return fmt.Errorf("can't save pack data: %v", err)
	}

	g.currentPackIndex.PackObject = oid.String()
	g.currentPackIndex = nil

	return nil
}

func (p *packManager) ensurePackIndexesLoaded() (map[string]*packIndex, error) {
	p.mu.RLock()
	pi := p.blockToIndex
	p.mu.RUnlock()
	if pi != nil {
		return pi, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	ch, cancel := p.objectManager.storage.ListBlocks(packObjectPrefix)
	defer cancel()

	t0 := time.Now()

	var wg sync.WaitGroup

	errors := make(chan error, parallelFetches)
	var mu sync.Mutex

	m := map[string][]byte{}
	totalSize := 0
	for i := 0; i < parallelFetches; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for b := range ch {
				if b.Error != nil {
					errors <- b.Error
					return
				}

				r, err := p.objectManager.Open(ObjectID{StorageBlock: b.BlockID})
				if err != nil {
					errors <- err
					return
				}

				data, err := ioutil.ReadAll(r)
				if err != nil {
					errors <- err
					return
				}

				mu.Lock()
				m[fmt.Sprintf("%16x", b.TimeStamp.UnixNano())] = data
				totalSize += len(data)
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	close(errors)

	// Propagate async errors, if any.
	for err := range errors {
		return nil, err
	}

	if false {
		log.Printf("loaded %v pack indexes (%v bytes) in %v", len(m), totalSize, time.Since(t0))
	}

	merged, err := loadMergedPackIndex(m)
	if err != nil {
		return nil, err
	}

	pi = make(map[string]*packIndex)
	for _, pck := range merged {
		for blockID := range pck.Items {
			pi[blockID] = pck
		}
	}

	p.blockToIndex = pi

	// log.Printf("loaded pack index with %v entries", len(p.blockToIndex))

	return pi, nil
}

func (p *packManager) newPackID() string {
	id := make([]byte, 8)
	rand.Read(id)
	return hex.EncodeToString(id)
}

func (p *packManager) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.finishCurrentPackLocked()
}
