package local_test

import (
	"bytes"
	"io/ioutil"
	. "launchpad.net/gocheck"
	"launchpad.net/juju-core/environs"
	"launchpad.net/juju-core/environs/local"
	"net/http"
)

type storageSuite struct{}

var _ = Suite(&storageSuite{})

// TestPersistence tests the adding, reading, listing and removing
// of files from the local storage.
func (s *storageSuite) TestPersistence(c *C) {
	// Non-standard port to avoid conflict with not-yet full 
	// closed listener in backend test.
	portNo := 60007
	listener, err := local.Listen(c.MkDir(), environName, "127.0.0.1", portNo)
	c.Assert(err, IsNil)
	defer listener.Close()
	storage := local.NewStorage("127.0.0.1", portNo)

	names := []string{
		"aa",
		"zzz/aa",
		"zzz/bb",
	}
	for _, name := range names {
		checkFileDoesNotExist(c, storage, name)
		checkPutFile(c, storage, name, []byte(name))
	}
	checkList(c, storage, "", names)
	checkList(c, storage, "a", []string{"aa"})
	checkList(c, storage, "zzz/", []string{"zzz/aa", "zzz/bb"})

	storage2 := local.NewStorage("127.0.0.1", portNo)
	for _, name := range names {
		checkFileHasContents(c, storage2, name, []byte(name))
	}

	// remove the first file and check that the others remain.
	err = storage2.Remove(names[0])
	c.Check(err, IsNil)

	// check that it's ok to remove a file twice.
	err = storage2.Remove(names[0])
	c.Check(err, IsNil)

	// ... and check it's been removed in the other environment
	checkFileDoesNotExist(c, storage, names[0])

	// ... and that the rest of the files are still around
	checkList(c, storage2, "", names[1:])

	for _, name := range names[1:] {
		err := storage2.Remove(name)
		c.Assert(err, IsNil)
	}

	// check they've all gone
	checkList(c, storage2, "", nil)
}

func checkList(c *C, storage environs.StorageReader, prefix string, names []string) {
	lnames, err := storage.List(prefix)
	c.Assert(err, IsNil)
	c.Assert(lnames, DeepEquals, names)
}

func checkPutFile(c *C, storage environs.StorageWriter, name string, contents []byte) {
	c.Logf("check putting file %s ...", name)
	err := storage.Put(name, bytes.NewBuffer(contents), int64(len(contents)))
	c.Assert(err, IsNil)
}

func checkFileDoesNotExist(c *C, storage environs.StorageReader, name string) {
	var notFoundError *environs.NotFoundError
	r, err := storage.Get(name)
	c.Assert(r, IsNil)
	c.Assert(err, FitsTypeOf, notFoundError)
}

func checkFileHasContents(c *C, storage environs.StorageReader, name string, contents []byte) {
	r, err := storage.Get(name)
	c.Assert(err, IsNil)
	c.Check(r, NotNil)
	defer r.Close()

	data, err := ioutil.ReadAll(r)
	c.Check(err, IsNil)
	c.Check(data, DeepEquals, contents)

	url, err := storage.URL(name)
	c.Assert(err, IsNil)

	resp, err := http.Get(url)
	c.Assert(err, IsNil)
	data, err = ioutil.ReadAll(resp.Body)
	c.Assert(err, IsNil)
	defer resp.Body.Close()
	c.Assert(resp.StatusCode, Equals, 200, Commentf("error response: %s", data))
	c.Check(data, DeepEquals, contents)
}
