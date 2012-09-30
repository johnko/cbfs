package main

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/gomemcached"
	"github.com/dustin/gomemcached/client"

	"github.com/couchbaselabs/cbfs/config"
)

const (
	blobPrefix   = "/.cbfs/blob/"
	nodePrefix   = "/.cbfs/nodes/"
	metaPrefix   = "/.cbfs/meta/"
	proxyPrefix  = "/.cbfs/viewproxy/"
	fetchPrefix  = "/.cbfs/fetch/"
	listPrefix   = "/.cbfs/list/"
	configPrefix = "/.cbfs/config/"
	zipPrefix    = "/.cbfs/zip/"
	fsckPrefix   = "/.cbfs/fsck/"
)

type storInfo struct {
	node string
	hs   string
	err  error
}

// Given a Reader, we produce a new reader that will duplicate the
// stream into the next available node and reproduce that content into
// another node.  Iff that node successfully stores the content, we
// return the hash it computed.
//
// The returned Reader must be consumed until the input EOFs or is
// closed.  The returned channel may yield a storInfo struct before
// it's closed.  If it's closed without yielding a storInfo, there are
// no remote nodes available.
func altStoreFile(r io.Reader) (io.Reader, <-chan storInfo) {
	bgch := make(chan storInfo, 1)

	nodes, err := findRemoteNodes()
	if err == nil && len(nodes) > 0 {
		r1, r2 := newMultiReader(r)
		r = r2

		go func() {
			defer close(bgch)

			rv := storInfo{node: nodes[0].Address()}

			rurl := "http://" +
				nodes[0].Address() + blobPrefix
			log.Printf("Piping secondary storage to %v",
				nodes[0].Address())

			preq, err := http.NewRequest("POST", rurl, r1)
			if err != nil {
				r1.CloseWithError(err)
				rv.err = err
				bgch <- rv
				return
			}

			client := http.Client{
				Transport: TimeoutTransport(time.Hour),
			}

			presp, err := client.Do(preq)
			if err == nil {
				if presp.StatusCode != 201 {
					rv.err = errors.New(presp.Status)
					r1.CloseWithError(rv.err)
					bgch <- rv
				}
				_, err := io.Copy(ioutil.Discard, presp.Body)
				if err == nil {
					rv.hs = presp.Header.Get("X-CBFS-Hash")
				}
				presp.Body.Close()
			} else {
				log.Printf("Error http'n to %v: %v", rurl, err)
			}
			rv.err = err
			bgch <- rv
		}()
	} else {
		close(bgch)
	}

	return r, bgch
}

func doPostRawBlob(w http.ResponseWriter, req *http.Request) {
	f, err := NewHashRecord(*root, "")
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}
	defer f.Close()

	sh, length, err := f.Process(req.Body)
	if err != nil {
		log.Printf("Error linking in raw hash: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	err = recordBlobOwnership(sh, length, true)
	if err != nil {
		log.Printf("Error recording blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	w.Header().Set("X-CBFS-Hash", sh)

	w.WriteHeader(201)
}

func putUserFile(w http.ResponseWriter, req *http.Request) {
	if strings.Contains(req.URL.Path, "//") {
		w.WriteHeader(400)
		fmt.Fprintf(w, "Too many slashes in the path name: %v",
			req.URL.Path)
		return
	}

	f, err := NewHashRecord(*root, req.Header.Get("X-CBFS-Hash"))
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		return
	}
	defer f.Close()

	r, bgch := altStoreFile(req.Body)

	h, length, err := f.Process(r)
	if err != nil {
		log.Printf("Error completing blob write: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error completing blob write: %v", err)
		return
	}

	log.Printf("Wrote %v -> %v", req.URL.Path, h)

	fm := fileMeta{
		Headers:  req.Header,
		OID:      h,
		Length:   length,
		Modified: time.Now().UTC(),
	}

	err = recordBlobOwnership(h, length, true)
	if err != nil {
		log.Printf("Error storing blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	if si, hasStuff := <-bgch; hasStuff {
		if si.err != nil || si.hs != h {
			log.Printf("Error in secondary store to %v: %v",
				si.node, si.err)
			w.WriteHeader(500)
			fmt.Fprintf(w, "Error creating secondary copy: %v\n%v",
				si.err, si.hs)
			return
		}
	}

	revs := globalConfig.DefaultVersionCount
	rheader := req.Header.Get("X-CBFS-KeepRevs")
	if rheader != "" {
		i, err := strconv.Atoi(rheader)
		if err == nil {
			revs = i
		}
	}

	err = storeMeta(resolvePath(req), fm, revs)
	if err != nil {
		log.Printf("Error storing file meta: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	if globalConfig.MinReplicas > 2 {
		// We're below min replica count.  Start fixing that
		// up immediately.
		go increaseReplicaCount(h, length, globalConfig.MinReplicas-2)
	}

	w.WriteHeader(201)
}

func putRawHash(w http.ResponseWriter, req *http.Request) {
	inputhash := minusPrefix(req.URL.Path, blobPrefix)

	if inputhash == "" {
		w.WriteHeader(400)
		w.Write([]byte("No oid specified"))
		return
	}

	f, err := NewHashRecord(*root, inputhash)
	if err != nil {
		log.Printf("Error writing tmp file: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}
	defer f.Close()

	sh, length, err := f.Process(req.Body)
	if err != nil {
		log.Printf("Error linking in raw hash: %v", err)
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
		return
	}

	err = recordBlobOwnership(inputhash, length, true)
	if err != nil {
		log.Printf("Error recording blob ownership: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error recording blob ownership: %v", err)
		return
	}

	w.Header().Set("X-CBFS-Hash", sh)

	w.WriteHeader(201)
}

func putConfig(w http.ResponseWriter, req *http.Request) {
	d := json.NewDecoder(req.Body)
	conf := cbfsconfig.CBFSConfig{}

	err := d.Decode(&conf)
	if err != nil {
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error reading config: %v", err)
		return
	}

	err = conf.StoreConfig(couchbase)
	if err != nil {
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error writing config: %v", err)
		return
	}

	err = updateConfig()
	if err != nil {
		log.Printf("Error fetching newly stored config: %v", err)
	}

	w.WriteHeader(204)
}

func doPut(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == configPrefix:
		putConfig(w, req)
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		putRawHash(w, req)
	case strings.HasPrefix(req.URL.Path, metaPrefix):
		putMeta(w, req, minusPrefix(req.URL.Path, metaPrefix))
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		putUserFile(w, req)
	}
}

func isResponseHeader(s string) bool {
	switch s {
	case "Content-Type", "Content-Length":
		return true
	}
	return false
}

func resolvePath(req *http.Request) string {
	path := req.URL.Path
	// Ignore /, but remove leading / from /blah
	for len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}

	if len(path) > 0 && path[len(path)-1] == '/' {
		path = path + "index.html"
	} else if len(path) == 0 {
		path = "index.html"
	}

	return path
}

func doHead(w http.ResponseWriter, req *http.Request) {
	path := resolvePath(req)
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	if req.FormValue("rev") != "" {
		w.WriteHeader(400)
		return
	}

	for k, v := range got.Headers {
		if isResponseHeader(k) {
			w.Header()[k] = v
		}
	}

	oldestRev := got.Revno
	if len(got.Previous) > 0 {
		oldestRev = got.Previous[0].Revno
	}

	w.Header().Set("X-CBFS-Revno", strconv.Itoa(got.Revno))
	w.Header().Set("X-CBFS-OldestRev", strconv.Itoa(oldestRev))
	w.Header().Set("Last-Modified",
		got.Modified.UTC().Format(http.TimeFormat))
	w.Header().Set("Etag", `"`+got.OID+`"`)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", got.Length))

	w.WriteHeader(200)
}

func doGetUserDoc(w http.ResponseWriter, req *http.Request) {
	path := resolvePath(req)
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	oid := got.OID
	respHeaders := got.Headers
	modified := got.Modified
	revno := got.Revno
	oldestRev := revno

	if len(got.Previous) > 0 {
		oldestRev = got.Previous[0].Revno
	}

	revnoStr := req.FormValue("rev")
	if revnoStr != "" {
		i, err := strconv.Atoi(revnoStr)
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, "Invalid revno")
			return
		}
		revno = i

		oid = ""
		for _, rev := range got.Previous {
			if rev.Revno == revno {
				oid = rev.OID
				modified = rev.Modified
				respHeaders = rev.Headers
				break
			}
		}
		if oid == "" {
			w.WriteHeader(410)
			fmt.Fprintf(w, "Don't have this file with rev %v", revno)
			return
		}
	}

	w.Header().Set("X-CBFS-Revno", strconv.Itoa(revno))
	w.Header().Set("X-CBFS-OldestRev", strconv.Itoa(oldestRev))

	inm := req.Header.Get("If-None-Match")
	if len(inm) > 2 {
		inm = inm[1 : len(inm)-1]
		if got.OID == inm {
			w.WriteHeader(304)
			return
		}
	}

	f, err := os.Open(hashFilename(*root, oid))
	if err != nil {
		getBlobFromRemote(w, oid, respHeaders, *cachePercentage)
		return
	}
	defer f.Close()

	for k, v := range respHeaders {
		if isResponseHeader(k) {
			w.Header()[k] = v
		}
	}

	w.Header().Set("Etag", `"`+oid+`"`)

	go recordBlobAccess(oid)
	http.ServeContent(w, req, path, modified, f)
}

func doServeRawBlob(w http.ResponseWriter, req *http.Request, oid string) {
	f, err := os.Open(hashFilename(*root, oid))
	if err != nil {
		w.WriteHeader(404)
		fmt.Fprintf(w, "Error opening blob: %v", err)
		removeBlobOwnershipRecord(oid, serverId)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")

	go recordBlobAccess(oid)
	http.ServeContent(w, req, "", time.Time{}, f)
}

func getBlobFromRemote(w http.ResponseWriter, oid string,
	respHeader http.Header, cachePerc int) error {

	// Find the owners of this blob
	ownership := BlobOwnership{}
	oidkey := "/" + oid
	err := couchbase.Get(oidkey, &ownership)
	if err != nil {
		log.Printf("Missing ownership record for OID: %v", oid)
		// Not sure 404 is the right response here
		w.WriteHeader(404)
		return err
	}

	nl := ownership.ResolveRemoteNodes()

	// Loop through the nodes that claim to own this blob
	// If we encounter any errors along the way, try the next node
	for _, sid := range nl {
		log.Printf("Trying to get %s from %s", oid, sid)

		resp, err := http.Get(sid.BlobURL(oid))
		if err != nil {
			log.Printf("Error reading oid %s from node %v",
				oid, sid)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			log.Printf("Error response %v from node %v",
				resp.Status, sid)
			continue
		}

		// Found one, set the headers and send it.  Keep a
		// local copy for good luck.

		for k, v := range respHeader {
			if isResponseHeader(k) {
				w.Header()[k] = v
			}
		}
		w.WriteHeader(200)
		writeTo := io.Writer(w)
		var hw *hashRecord

		if cachePerc == 100 || (cachePerc > rand.Intn(100) &&
			availableSpace() > uint64(ownership.Length)) {
			hw, err = NewHashRecord(*root, oid)
			if err == nil {
				writeTo = io.MultiWriter(hw, w)
			} else {
				hw = nil
			}
		}

		length, err := io.Copy(writeTo, resp.Body)

		if err != nil {
			log.Printf("Failed to write from remote stream %v", err)
			return err
		} else {
			// A successful copy with a working hash
			// record means we should link in and record
			// our copy of this file.
			if hw != nil {
				_, err = hw.Finish()
				if err == nil {
					go recordBlobOwnership(oid, length,
						true)
				}
			}
		}

		return nil
	}

	//if we got to this point, no node in the list actually had it
	log.Printf("Don't have hash file: %v and no remote nodes could help",
		oid)
	w.WriteHeader(500)
	fmt.Fprintf(w, "Cannot locate blob %v", oid)
	return fmt.Errorf("Can't locate blob %v", oid)
}

func doList(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(200)
	explen := getHash().Size() * 2
	filepath.Walk(*root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && !strings.HasPrefix(info.Name(), "tmp") &&
			len(info.Name()) == explen {
			_, e := w.Write([]byte(info.Name() + "\n"))
			return e
		}
		return nil
	})
}

func putMeta(w http.ResponseWriter, req *http.Request, path string) {
	got := fileMeta{}
	casid := uint64(0)
	err := couchbase.Gets(path, &got, &casid)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	r := json.RawMessage{}
	err = json.NewDecoder(req.Body).Decode(&r)
	if err != nil {
		w.WriteHeader(400)
		w.Write([]byte(err.Error()))
		return
	}

	got.Userdata = &r
	b := mustEncode(&got)

	err = couchbase.Do(path, func(mc *memcached.Client, vb uint16) error {
		req := &gomemcached.MCRequest{
			Opcode:  gomemcached.SET,
			VBucket: vb,
			Key:     []byte(path),
			Cas:     casid,
			Opaque:  0,
			Extras:  []byte{0, 0, 0, 0, 0, 0, 0, 0},
			Body:    b}
		resp, err := mc.Send(req)
		if err != nil {
			return err
		}
		if resp.Status != gomemcached.SUCCESS {
			return resp
		}
		return nil
	})

	if err == nil {
		w.WriteHeader(201)
	} else {
		w.WriteHeader(500)
		w.Write([]byte(err.Error()))
	}
}

func doGetMeta(w http.ResponseWriter, req *http.Request, path string) {
	got := fileMeta{}
	err := couchbase.Get(path, &got)
	if err != nil {
		log.Printf("Error getting file %#v: %v", path, err)
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	if got.Userdata == nil {
		w.Write([]byte("{}"))
	} else {
		w.Write(*got.Userdata)
	}
}

func doListNodes(w http.ResponseWriter, req *http.Request) {

	nl, err := findAllNodes()
	if err != nil {
		log.Printf("Error executing nodes view: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error generating node list: %v", err)
		return
	}

	respob := map[string]interface{}{}
	for _, node := range nl {
		age := time.Since(node.Time)
		respob[node.name] = map[string]interface{}{
			"size":      node.storageSize,
			"addr":      node.Address(),
			"hbtime":    node.Time,
			"hbage_ms":  age.Nanoseconds() / 1e6,
			"hbage_str": age.String(),
			"hash":      node.Hash,
			"used":      node.Used,
			"free":      node.Free,
			"addr_raw":  node.Addr,
			"bindaddr":  node.BindAddr,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(mustEncode(respob))
}

func canGzip(req *http.Request) bool {
	acceptable := req.Header.Get("accept-encoding")
	return strings.Contains(acceptable, "gzip")
}

func proxyViewRequest(w http.ResponseWriter, req *http.Request,
	path string) {

	node := couchbase.Nodes[rand.Intn(len(couchbase.Nodes))]
	u, err := url.Parse(node.CouchAPIBase)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	u.Path = "/" + path
	u.RawQuery = req.URL.RawQuery

	client := &http.Client{
		Transport: TimeoutTransport(*viewTimeout),
	}

	res, err := client.Get(u.String())
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	for k, vs := range res.Header {
		w.Header()[k] = vs
	}

	output := io.Writer(w)

	if canGzip(req) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		output = gz
	}
	w.WriteHeader(res.StatusCode)

	io.Copy(output, res.Body)
}

type captureResponseWriter struct {
	w          io.Writer
	hdr        http.Header
	statusCode int
}

func (c *captureResponseWriter) Header() http.Header {
	return c.hdr
}

func (c *captureResponseWriter) Write(b []byte) (int, error) {
	return c.w.Write(b)
}

func (c *captureResponseWriter) WriteHeader(code int) {
	c.statusCode = code
}

func doFetchDoc(w http.ResponseWriter, req *http.Request,
	path string) {

	ownership := BlobOwnership{}
	oidkey := "/" + path
	err := couchbase.Get(oidkey, &ownership)
	if err != nil {
		log.Printf("Missing ownership record for OID: %v",
			path)
		// Not sure 404 is the right response here
		w.WriteHeader(404)
		return
	}

	if availableSpace() < uint64(ownership.Length) {
		w.WriteHeader(500)
		w.Write([]byte("No free space available."))
		log.Printf("Someone asked me to get %v, but I'm out of space",
			path)
		return
	}

	queueBlobFetch(path, req.Header.Get("X-Prevnode"))
	w.WriteHeader(202)
}

func doListDocs(w http.ResponseWriter, req *http.Request,
	path string) {

	// trim off trailing slash early so we handle them consistently
	if strings.HasSuffix(path, "/") {
		path = path[0 : len(path)-1]
	}

	includeMeta := req.FormValue("includeMeta")
	depthString := req.FormValue("depth")
	depth := 1
	if depthString != "" {
		i, err := strconv.Atoi(depthString)
		if err != nil {
			w.WriteHeader(400)
			fmt.Fprintf(w, "Error processing depth parameter: %v", err)
			return
		}
		depth = i
	}

	fl, err := listFiles(path, includeMeta == "true", depth)
	if err != nil {
		log.Printf("Error executing file browse view: %v", err)
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error generating file list: %v", err)
		return
	}

	if len(fl.Dirs) == 0 && len(fl.Files) == 0 {
		w.WriteHeader(404)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)

	e := json.NewEncoder(w)
	err = e.Encode(fl)
	if err != nil {
		log.Printf("Error writing json stream: %v", err)
	}
}

func doGetConfig(w http.ResponseWriter, req *http.Request) {
	err := updateConfig()
	if err != nil {
		w.WriteHeader(500)
		fmt.Fprintf(w, "Error updating config: %v", err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)

	e := json.NewEncoder(w)
	err = e.Encode(&globalConfig)
	if err != nil {
		log.Printf("Error sending config: %v", err)
	}
}

func doGet(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == blobPrefix:
		doList(w, req)
	case req.URL.Path == nodePrefix:
		doListNodes(w, req)
	case req.URL.Path == configPrefix:
		doGetConfig(w, req)
	case strings.HasPrefix(req.URL.Path, fetchPrefix):
		doFetchDoc(w, req,
			minusPrefix(req.URL.Path, fetchPrefix))
	case strings.HasPrefix(req.URL.Path, metaPrefix):
		doGetMeta(w, req,
			minusPrefix(req.URL.Path, metaPrefix))
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		doServeRawBlob(w, req, minusPrefix(req.URL.Path, blobPrefix))
	case *enableViewProxy && strings.HasPrefix(req.URL.Path, proxyPrefix):
		proxyViewRequest(w, req, minusPrefix(req.URL.Path, proxyPrefix))
	case strings.HasPrefix(req.URL.Path, listPrefix):
		doListDocs(w, req, minusPrefix(req.URL.Path, listPrefix))
	case strings.HasPrefix(req.URL.Path, zipPrefix):
		doZipDocs(w, req, minusPrefix(req.URL.Path, zipPrefix))
	case strings.HasPrefix(req.URL.Path, fsckPrefix):
		dofsck(w, req, minusPrefix(req.URL.Path, fsckPrefix))
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		doGetUserDoc(w, req)
	}
}

func minusPrefix(s, prefix string) string {
	return s[len(prefix):]
}

func doDeleteOID(w http.ResponseWriter, req *http.Request) {
	oid := minusPrefix(req.URL.Path, blobPrefix)

	ob, err := getBlobOwnership(oid)
	if err == nil {
		n, t := ob.mostRecent()
		if time.Since(t) < time.Hour {
			log.Printf("%v was referenced within the last hour by %v, ignoring",
				oid, n)
			w.WriteHeader(400)
			return
		}
	}
	err = removeObject(oid)
	if err == nil {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
	}
}

func doDeleteUserDoc(w http.ResponseWriter, req *http.Request) {
	err := couchbase.Delete(resolvePath(req))
	if err == nil {
		w.WriteHeader(204)
	} else {
		w.WriteHeader(404)
		w.Write([]byte(err.Error()))
	}
}

func doDelete(w http.ResponseWriter, req *http.Request) {
	switch {
	case strings.HasPrefix(req.URL.Path, blobPrefix):
		doDeleteOID(w, req)
	case strings.HasPrefix(req.URL.Path, "/.cbfs/"):
		w.WriteHeader(400)
	default:
		doDeleteUserDoc(w, req)
	}
}

func doPost(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == blobPrefix {
		doPostRawBlob(w, req)
	} else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func httpHandler(w http.ResponseWriter, req *http.Request) {
	defer req.Body.Close()
	switch req.Method {
	case "PUT":
		doPut(w, req)
	case "POST":
		doPost(w, req)
	case "GET":
		doGet(w, req)
	case "HEAD":
		doHead(w, req)
	case "DELETE":
		doDelete(w, req)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
