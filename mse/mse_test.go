package mse

import (
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"io"
	"io/ioutil"
	"net"
	"sync"
	"testing"

	"github.com/bradfitz/iter"
	"github.com/stretchr/testify/require"
)

func TestReadUntil(t *testing.T) {
	test := func(data, until string, leftover int, expectedErr error) {
		r := bytes.NewReader([]byte(data))
		err := readUntil(r, []byte(until))
		if err != expectedErr {
			t.Fatal(err)
		}
		if r.Len() != leftover {
			t.Fatal(r.Len())
		}
	}
	test("feakjfeafeafegbaabc00", "abc", 2, nil)
	test("feakjfeafeafegbaadc00", "abc", 0, io.EOF)
}

func TestSuffixMatchLen(t *testing.T) {
	test := func(a, b string, expected int) {
		actual := suffixMatchLen([]byte(a), []byte(b))
		if actual != expected {
			t.Fatalf("expected %d, got %d for %q and %q", expected, actual, a, b)
		}
	}
	test("hello", "world", 0)
	test("hello", "lo", 2)
	test("hello", "llo", 3)
	test("hello", "hell", 0)
	test("hello", "helloooo!", 5)
	test("hello", "lol!", 2)
	test("hello", "mondo", 0)
	test("mongo", "webscale", 0)
	test("sup", "person", 1)
}

func handshakeTest(t testing.TB, ia []byte, aData, bData string) {
	a, b := net.Pipe()
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		a, err := InitiateHandshake(a, []byte("yep"), ia)
		if err != nil {
			t.Fatal(err)
			return
		}
		go a.Write([]byte(aData))

		var msg [20]byte
		n, _ := a.Read(msg[:])
		if n != len(bData) {
			t.FailNow()
		}
		// t.Log(string(msg[:n]))
	}()
	go func() {
		defer wg.Done()
		b, err := ReceiveHandshake(b, [][]byte{[]byte("nope"), []byte("yep"), []byte("maybe")})
		if err != nil {
			t.Fatal(err)
			return
		}
		go b.Write([]byte(bData))
		// Need to be exact here, as there are several reads, and net.Pipe is
		// most synchronous.
		msg := make([]byte, len(ia)+len(aData))
		n, _ := io.ReadFull(b, msg[:])
		if n != len(msg) {
			t.FailNow()
		}
		// t.Log(string(msg[:n]))
	}()
	wg.Wait()
	a.Close()
	b.Close()
}

func allHandshakeTests(t testing.TB) {
	handshakeTest(t, []byte("jump the gun, "), "hello world", "yo dawg")
	handshakeTest(t, nil, "hello world", "yo dawg")
	handshakeTest(t, []byte{}, "hello world", "yo dawg")
}

func TestHandshake(t *testing.T) {
	allHandshakeTests(t)
	t.Logf("crypto provides encountered: %s", cryptoProvidesCount)
}

func BenchmarkHandshake(b *testing.B) {
	for range iter.N(b.N) {
		allHandshakeTests(b)
	}
}

type trackReader struct {
	r io.Reader
	n int64
}

func (tr *trackReader) Read(b []byte) (n int, err error) {
	n, err = tr.r.Read(b)
	tr.n += int64(n)
	return
}

func TestReceiveRandomData(t *testing.T) {
	tr := trackReader{rand.Reader, 0}
	_, err := ReceiveHandshake(readWriter{&tr, ioutil.Discard}, nil)
	// No skey matches
	require.Error(t, err)
	// Establishing S, and then reading the maximum padding for giving up on
	// synchronizing.
	require.EqualValues(t, 96+532, tr.n)
}

func BenchmarkPipe(t *testing.B) {
	key := make([]byte, 20)
	n, _ := rand.Read(key)
	require.Equal(t, len(key), n)
	var buf bytes.Buffer
	c, err := rc4.NewCipher(key)
	require.NoError(t, err)
	r := cipherReader{
		c: c,
		r: &buf,
	}
	c, err = rc4.NewCipher(key)
	require.NoError(t, err)
	w := cipherWriter{
		c: c,
		w: &buf,
	}
	a := make([]byte, 0x1000)
	n, _ = io.ReadFull(rand.Reader, a)
	require.Equal(t, len(a), n)
	b := make([]byte, len(a))
	t.SetBytes(int64(len(a)))
	t.ResetTimer()
	for range iter.N(t.N) {
		n, _ = w.Write(a)
		if n != len(a) {
			t.FailNow()
		}
		n, _ = r.Read(b)
		if n != len(b) {
			t.FailNow()
		}
		if !bytes.Equal(a, b) {
			t.FailNow()
		}
	}
}
