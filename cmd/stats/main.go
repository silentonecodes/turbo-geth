package main

import (
	"encoding/csv"
	"fmt"
	"github.com/ledgerwatch/bolt"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/common/dbutils"
	"github.com/ledgerwatch/turbo-geth/ethdb"
	"log"
	"os"
	"sort"
	"strconv"
)

func main() {
	calculateSizes()
	//calculateIndexSize()
	//testMigrate()
	//migrateAccountIndexes()
	//migrateStorageIndexes()
	//calculateSizeOfAccounts()
	//storageFormatDiff3()
	//migragteCompressionOfBlocks()
	//copyCodeContracts()
	//testMigrate()
	//storageFormatDiff2()
	//collectStorageNumOfDuplicate()
}
func calculateIndexSize() {
	db, err := ethdb.NewBoltDatabase("/media/b00ris/nvme/thin_last/geth/chaindata")
	if err != nil {
		log.Fatal(err)
	}
	f:=func(bucket []byte) (uint64, uint64) {
		var origSize, newSize uint64
		i:=uint64(0)
		j:=0
		var (
			currentKey, currentValue []byte
			done                     bool
		)
		for !done {
			err := db.DB().Update(func(tx *bolt.Tx) error {
				accontHisory := tx.Bucket(bucket)
				accountHistoryCurson := accontHisory.Cursor()

				if currentKey == nil {
					currentKey, currentValue = accountHistoryCurson.First()
				} else {
					currentKey, currentValue = accountHistoryCurson.Seek(currentKey)
				}
				for currentKey != nil {
					fmt.Println(j, i, string(bucket), common.Bytes2Hex(currentKey))
					index:=dbutils.WrapHistoryIndex(currentValue)
					origSize+=uint64(len(currentValue))

					dec,err:=index.Decode()
					if err!=nil {
						log.Fatal("err on decode", common.Bytes2Hex(currentKey), err)
					}
					mp:=make(map[uint64]struct{})
					for _,val:=range dec {
						mp[val] = struct{}{}
					}
					newSlice:=make([]uint64, 0, len(mp))
					for i:=range mp {
						newSlice=append(newSlice, i)
					}
					sort.Slice(newSlice, func(i, j int) bool {
						return newSlice[i]<newSlice[j]
					})
					newIndex:=dbutils.NewHistoryIndex()
					for _, val:=range newSlice {
						newIndex = newIndex.Append(val)
					}
					err = accontHisory.Put(currentKey, *newIndex)
					newSize+=uint64(len(*newIndex))
					if err!=nil {
						log.Fatal("err on put", common.Bytes2Hex(currentKey), err)
					}
					i++
					j++
					if i > 3000 {
						i=0
						return nil
					}
					currentKey, currentValue = accountHistoryCurson.Next()
				}
				if currentKey==nil {
					done=true
				}
				return nil
			})
			if err!=nil {
				log.Fatal("Err on update", common.Bytes2Hex(currentKey),  err)
			}
		}
		return origSize, newSize
	}
	one, two:=f(dbutils.AccountsHistoryBucket)
	three,four:=f(dbutils.StorageHistoryBucket)
	fmt.Println("Results:")
	fmt.Println(one, two)
	fmt.Println(three, four)
}


func calculateSizes() {
	db, err := ethdb.NewBoltDatabase("/media/b00ris/nvme/thin_last/geth/chaindata")
	if err != nil {
		log.Fatal(err)
	}
	fStorage,err:=os.Create("/media/b00ris/nvme/thin_last/geth/index_sizes_storage.csv")
	if err!=nil {
		log.Fatal("err", err)
	}
	fAcc,err:=os.Create("/media/b00ris/nvme/thin_last/geth/index_sizes_acc.csv")
	if err!=nil {
		log.Fatal("err", err)
	}
	csvAcc:=csv.NewWriter(fAcc)
	err = csvAcc.Write([]string{"key", "ln"})
	if err!=nil {
		log.Fatal("Write err", err)
	}
	csvStorage:=csv.NewWriter(fStorage)
	err = csvStorage.Write([]string{"key", "ln"})
	i:=0
	j:=0
	maxLenAcc:=0
	maxLenSt:=0
	db.Walk(dbutils.AccountsHistoryBucket, []byte{}, 0, func(k, v []byte) (b bool, e error) {
		if i>10000 {
			fmt.Println(j)
			i=0
		}
		if len(v)> maxLenAcc {
			maxLenAcc=len(v)
		}
		err = csvAcc.Write([]string{common.Bytes2Hex(k), strconv.Itoa(len(v))})
		if err!=nil {
			log.Fatal("Write err", err, common.Bytes2Hex(k))
		}

		return true, nil
	})
	i=0
	j=0
	db.Walk(dbutils.StorageHistoryBucket, []byte{}, 0, func(k, v []byte) (b bool, e error) {
		if i>10000 {
			fmt.Println(j)
			i=0
		}
		if len(v)> maxLenSt {
			maxLenSt=len(v)
		}
		err = csvStorage.Write([]string{common.Bytes2Hex(k), strconv.Itoa(len(v))})
		if err!=nil {
			log.Fatal("Write err", err, common.Bytes2Hex(k))
		}

		return true, nil
	})

	fmt.Println("Results:")
	fmt.Println("maxLenAcc:", maxLenAcc)
	fmt.Println("maxLenSt:", maxLenSt)

}


/*

Current size 75989100632
Dict size 43257146833
Dict errors 0
(map[uint64]uint64) (len=2) {
 (uint64) 18374686479671623679: (uint64) 935166687,
 (uint64) 18302628885633695743: (uint64) 12
}

Current size 75989100632
Exp size 65702267087
Exp errors 0
Dict size 56629648083
Dict errors 0



rlp size       126317329945
gzip rlp size  84384714811
gzip rlp2 size 88306259484

*/
