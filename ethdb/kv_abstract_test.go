package ethdb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"github.com/ledgerwatch/turbo-geth/ethdb/remote/remotedbserver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedTx(t *testing.T) {
	ctx := context.Background()

	writeDBs := []ethdb.KV{
		ethdb.NewBolt().InMem().MustOpen(ctx),
		ethdb.NewBolt().InMem().MustOpen(ctx), // for remote db
		ethdb.NewBadger().InMem().MustOpen(ctx),
	}

	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()

	readDBs := []ethdb.KV{
		writeDBs[0],
		ethdb.NewRemote().InMem(clientIn, clientOut).MustOpen(ctx),
		writeDBs[2],
	}

	serverCtx, serverCancel := context.WithCancel(ctx)
	go func() {
		_ = remotedbserver.Server(serverCtx, writeDBs[1], serverIn, serverOut, nil)
	}()

	defer func() {
		for _, db := range writeDBs {
			db.Close()
		}
		for _, db := range readDBs {
			db.Close()
		}

		serverIn.Close()
		serverOut.Close()
		clientIn.Close()
		clientOut.Close()

		serverCancel()
	}()

	for _, db := range writeDBs {
		db := db
		if err := db.Update(ctx, func(tx ethdb.Tx) error {
			b := tx.Bucket(dbutils.CurrentStateBucket)
			for i := uint8(0); i < 10; i++ {
				require.NoError(t, b.Put([]byte{i}, []byte{1}))
			}
			require.NoError(t, b.Put([]byte{0, 1}, []byte{1}))
			require.NoError(t, b.Put([]byte{0, 0, 1}, []byte{1}))
			return nil
		}); err != nil {
			require.NoError(t, err)
		}
	}

	for _, db := range readDBs {
		db := db
		msg := fmt.Sprintf("%T", db)

		t.Run("NoValues iterator "+msg, func(t *testing.T) {
			testNoValuesIterator(t, db)
		})
		t.Run("ctx cancel "+msg, func(t *testing.T) {
			t.Skip("probably need enable after go 1.4")
			testCtxCancel(t, db)
		})
		t.Run("filter "+msg, func(t *testing.T) {
			testPrefixFilter(t, db)
		})
	}
}

func testPrefixFilter(t *testing.T, db ethdb.KV) {
	assert := assert.New(t)

	if err := db.View(context.Background(), func(tx ethdb.Tx) error {
		b := tx.Bucket(dbutils.CurrentStateBucket)
		c := b.Cursor().Prefix([]byte{2})
		counter := 0
		for k, _, err := c.First(); k != nil || err != nil; k, _, err = c.Next() {
			if err != nil {
				return err
			}
			counter++
		}
		assert.Equal(1, counter)

		counter = 0
		if err := c.Walk(func(_, _ []byte) (bool, error) {
			counter++
			return true, nil
		}); err != nil {
			return err
		}
		assert.Equal(1, counter)

		k, _, err := c.Seek([]byte{2})
		assert.NoError(err)
		assert.Equal([]byte{2}, k)

		c = b.Cursor()
		counter = 0
		for k, _, err := c.First(); k != nil || err != nil; k, _, err = c.Next() {
			if err != nil {
				return err
			}
			counter++
		}
		assert.Equal(12, counter)

		counter = 0
		if err := c.Walk(func(_, _ []byte) (bool, error) {
			counter++
			return true, nil
		}); err != nil {
			return err
		}
		assert.Equal(12, counter)

		k, _, err = c.Seek([]byte{2})
		assert.NoError(err)
		assert.Equal([]byte{2}, k)
		return nil
	}); err != nil {
		assert.NoError(err)
	}

}
func testCtxCancel(t *testing.T, db ethdb.KV) {
	assert := assert.New(t)
	cancelableCtx, cancel := context.WithTimeout(context.Background(), time.Microsecond)
	defer cancel()

	if err := db.View(cancelableCtx, func(tx ethdb.Tx) error {
		c := tx.Bucket(dbutils.CurrentStateBucket).Cursor()
		for {
			for k, _, err := c.First(); k != nil || err != nil; k, _, err = c.Next() {
				if err != nil {
					return err
				}
			}
		}
	}); err != nil {
		assert.True(errors.Is(context.DeadlineExceeded, err))
	}
}

func testNoValuesIterator(t *testing.T, db ethdb.KV) {
	assert, ctx := assert.New(t), context.Background()

	if err := db.View(ctx, func(tx ethdb.Tx) error {
		b := tx.Bucket(dbutils.CurrentStateBucket)
		c := b.Cursor()

		k, _, err := c.First()
		assert.NoError(err)
		assert.Equal([]byte{0}, k)
		k, _, err = c.Next()
		assert.NoError(err)
		assert.Equal([]byte{0, 0, 1}, k)
		k, _, err = c.Next()
		assert.NoError(err)
		assert.Equal([]byte{0, 1}, k)
		k, _, err = c.Next()
		assert.NoError(err)
		assert.Equal([]byte{1}, k)
		k, _, err = c.Seek([]byte{0, 1})
		assert.NoError(err)
		assert.Equal([]byte{0, 1}, k)
		k, _, err = c.Seek([]byte{2})
		assert.NoError(err)
		assert.Equal([]byte{2}, k)
		k, _, err = c.Seek([]byte{99})
		assert.NoError(err)
		assert.Nil(k)

		c2 := b.Cursor().NoValues()

		k, _, err = c2.First()
		assert.NoError(err)
		assert.Equal([]byte{0}, k)
		k, _, err = c2.Next()
		assert.NoError(err)
		assert.Equal([]byte{0, 0, 1}, k)
		k, _, err = c2.Next()
		assert.NoError(err)
		assert.Equal([]byte{0, 1}, k)
		k, _, err = c2.Next()
		assert.NoError(err)
		assert.Equal([]byte{1}, k)
		k, _, err = c2.Seek([]byte{0, 1})
		assert.NoError(err)
		assert.Equal([]byte{0, 1}, k)
		k, _, err = c2.Seek([]byte{0})
		assert.NoError(err)
		assert.Equal([]byte{0}, k)
		k, _, err = c.Seek([]byte{2})
		assert.NoError(err)
		assert.Equal([]byte{2}, k)
		k, _, err = c.Seek([]byte{99})
		assert.NoError(err)
		assert.Nil(k)

		return nil
	}); err != nil {
		assert.NoError(err)
	}
}
