package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	sealing "github.com/filecoin-project/lotus/extern/storage-sealing"
	saproof2 "github.com/filecoin-project/specs-actors/v2/actors/runtime/proof"

	"github.com/docker/go-units"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/minio/blake2b-simd"
	"github.com/mitchellh/go-homedir"
	"github.com/urfave/cli/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	paramfetch "github.com/filecoin-project/go-paramfetch"
	"github.com/filecoin-project/go-state-types/abi"
	lcli "github.com/filecoin-project/lotus/cli"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper"
	"github.com/filecoin-project/lotus/extern/sector-storage/ffiwrapper/basicfs"
	"github.com/filecoin-project/lotus/extern/sector-storage/storiface"
	"github.com/filecoin-project/specs-storage/storage"

	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/genesis"
)

var log = logging.Logger("lotus-bench")

type BenchResults struct {
	EnvVar map[string]string

	SectorSize   abi.SectorSize
	SectorNumber int

	SealingSum     SealingResult
	SealingResults []SealingResult

	PostGenerateCandidates time.Duration
	PostWinningProofCold   time.Duration
	PostWinningProofHot    time.Duration
	VerifyWinningPostCold  time.Duration
	VerifyWinningPostHot   time.Duration

	PostWindowProofCold  time.Duration
	PostWindowProofHot   time.Duration
	VerifyWindowPostCold time.Duration
	VerifyWindowPostHot  time.Duration
}

func (bo *BenchResults) SumSealingTime() error {
	if len(bo.SealingResults) <= 0 {
		return xerrors.Errorf("BenchResults SealingResults len <= 0")
	}
	if len(bo.SealingResults) != bo.SectorNumber {
		return xerrors.Errorf("BenchResults SealingResults len(%d) != bo.SectorNumber(%d)", len(bo.SealingResults), bo.SectorNumber)
	}

	for _, sealing := range bo.SealingResults {
		bo.SealingSum.AddPiece += sealing.AddPiece
		bo.SealingSum.PreCommit1 += sealing.PreCommit1
		bo.SealingSum.PreCommit2 += sealing.PreCommit2
		bo.SealingSum.Commit1 += sealing.Commit1
		bo.SealingSum.Commit2 += sealing.Commit2
		bo.SealingSum.Verify += sealing.Verify
		bo.SealingSum.Unseal += sealing.Unseal
	}
	return nil
}

type SealingResult struct {
	AddPiece   time.Duration
	PreCommit1 time.Duration
	PreCommit2 time.Duration
	Commit1    time.Duration
	Commit2    time.Duration
	Verify     time.Duration
	Unseal     time.Duration
}

// section-benchmark
type PreCommit1In struct {
	Size     uint64
	PieceCID string
}
type PreCommit2In struct {
	Size   uint64
	Data   []byte
	Ticket []byte
}
type Commit1In struct {
	Size     uint64
	Unsealed string
	Sealed   string
}
type Commit2In struct {
	SectorNum  int64
	Phase1Out  []byte
	SectorSize uint64
}

func main() {
	logging.SetLogLevel("*", "INFO")

	log.Info("Starting lotus-bench")

	app := &cli.App{
		Name:    "lotus-bench",
		Usage:   "Benchmark performance of lotus on your hardware",
		Version: build.UserVersion(),
		Commands: []*cli.Command{
			proveCmd,
			sealBenchCmd,
			importBenchCmd,
			// section-benchmark
			addPieceCmd,
			preCommit1Cmd,
			preCommit2Cmd,
			commit1Cmd,
			commit2Cmd,
			// sector-recovery  //powerd by https://www.pocyc.com
			recoveryCmd,
		},
	}

	if err := app.Run(os.Args); err != nil {
		log.Warnf("%+v", err)
		return
	}
}

// section-benchmark
var addPieceCmd = &cli.Command{
	Name:  "addpiece",
	Usage: "Benchmark AddPiece",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "Path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "sector-size",
			Value: "512MiB",
			Usage: "size of the sectors in bytes, i.e. 32GiB",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Value: "t01000",
			Usage: "miner address",
		},
		&cli.StringFlag{
			Name:  "sector-id",
			Value: "10",
			Usage: "sector number",
		},
	},
	Action: func(c *cli.Context) error {
		// storage-dir
		sdir, err := homedir.Expand(c.String("storage-dir"))
		if err != nil {
			return err
		}

		err = os.MkdirAll(sdir, 0775) //nolint:gosec
		if err != nil {
			return xerrors.Errorf("creating sectorbuilder dir: %w", err)
		}

		// sector size
		sectorSizeInt, err := units.RAMInBytes(c.String("sector-size"))
		if err != nil {
			return err
		}
		sectorSize := abi.SectorSize(sectorSizeInt)

		// sector id
		sectoridInt, err := units.RAMInBytes(c.String("sector-id"))
		if err != nil {
			return err
		}
		sectorid := abi.SectorNumber(sectoridInt)

		// miner address
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		var sectorfullname = "s-" + c.String("miner-id") + "-" + c.String("sector-id")

		// config proofs
		sbfs := &basicfs.Provider{
			Root: sdir,
		}
		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		var sealTimings SealingResult
		start := time.Now()

		// start AddPiece
		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,
				Number: sectorid, //abi.SectorNumber(10),
			},
			ProofType: spt(sectorSize),
		}

		log.Infof("[%d] Writing piece into sector...", 1)
		log.Infof("addPieceCmd sid=%v", sid)

		pi, err := sb.AddPiece(context.TODO(), sid, nil, abi.PaddedPieceSize(sectorSize).Unpadded(), sealing.NewNullReader(abi.PaddedPieceSize(sectorSize).Unpadded()))
		if err != nil {
			return err
		}

		addpiece := time.Now()

		p1in := PreCommit1In{
			Size:     uint64(pi.Size),
			PieceCID: pi.PieceCID.String(),
		}

		b, err := json.Marshal(&p1in)
		if err != nil {
			return err
		}

		if err := ioutil.WriteFile("p1in.json", b, 0664); err != nil {
			return err
		}

		sealTimings.AddPiece = addpiece.Sub(start)
		fmt.Printf("----\nresults (v28) (%d)\n", sectorSize)
		fmt.Printf("seal: addPiece: %s (%s)\n", sealTimings.AddPiece, bps(sectorSize, 1, sealTimings.AddPiece))

		fmt.Printf("---- \n")
		fmt.Printf("%v \n", sectorfullname)
		fmt.Printf("sid=%v \n", sid)
		fmt.Printf("p1in=%v \n", p1in)

		return nil
	},
}
var preCommit1Cmd = &cli.Command{
	Name:  "precommit1",
	Usage: "Benchmark PreCommit1",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "Path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Value: "t01000",
			Usage: "miner address",
		},
		&cli.StringFlag{
			Name:  "sector-id",
			Value: "10",
			Usage: "sector number",
		},
		&cli.StringFlag{
			Name:  "ticket",
			Value: "",
			Usage: "ticket",
		},
	},
	Action: func(c *cli.Context) error {
		// storage-dir
		sdir, err := homedir.Expand(c.String("storage-dir"))
		if err != nil {
			return err
		}

		// miner address
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		// sector id
		sectoridInt, err := units.RAMInBytes(c.String("sector-id"))
		if err != nil {
			return err
		}
		sectorid := abi.SectorNumber(sectoridInt)

		// ticket
		var ticketStr = c.String("ticket")
		if len(ticketStr) != 64 {
			//return xerrors.Errorf("ticket len is fault.")
			ticketStr = "e8aa3646eade2bafbc58b9210cb8eef1b4dedf17359bc9c391f2aaf0fad0a7a5"
		}
		ticketHex, err := hex.DecodeString(ticketStr)
		if err != nil {
			return err
		}
		ticket := abi.SealRandomness(ticketHex[:])

		var sectorfullname = "s-" + c.String("miner-id") + "-" + c.String("sector-id")

		// read p1in
		inb, err := ioutil.ReadFile("p1in.json")
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var p1in PreCommit1In
		if err := json.Unmarshal(inb, &p1in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,      //abi.ActorID(1000),
				Number: sectorid, //abi.SectorNumber(10),
			},
			ProofType: spt(abi.SectorSize(p1in.Size)),
		}

		_cid, err := cid.Decode(p1in.PieceCID)
		if err != nil {
			return err
		}

		pi := abi.PieceInfo{
			Size:     abi.PaddedPieceSize(p1in.Size),
			PieceCID: _cid,
		}

		// config proofs
		sbfs := &basicfs.Provider{
			Root: sdir,
		}
		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		var sealTimings SealingResult
		pc1Start := time.Now()

		// start PreCommit1
		log.Infof("[%d] Running replication(1)...", 1)
		pieces := []abi.PieceInfo{pi}

		log.Infof("preCommit1Cmd sid=%v", sid)
		log.Infof("preCommit1Cmd ticket=%v ticket-hex=%v", ticketStr, ticket)
		log.Infof("preCommit1Cmd pieces=%v", pieces)

		pc1o, err := sb.SealPreCommit1(context.TODO(), sid, ticket, pieces)
		if err != nil {
			return xerrors.Errorf("commit: %w", err)
		}

		precommit1 := time.Now()

		p2in := PreCommit2In{
			Data:   pc1o,
			Size:   p1in.Size,
			Ticket: ticket,
		}

		b, err := json.Marshal(&p2in)
		if err != nil {
			return err
		}

		if err := ioutil.WriteFile("p2in.json", b, 0664); err != nil {
			return err
		}

		sealTimings.PreCommit1 = precommit1.Sub(pc1Start)
		fmt.Printf("----\nresults (v28) (%d)\n", p1in.Size)
		fmt.Printf("seal: preCommit phase 1: %s (%s)\n", sealTimings.PreCommit1, bps(abi.SectorSize(p1in.Size), 1, sealTimings.PreCommit1))

		fmt.Printf("---- \n")
		fmt.Printf("%v \n", sectorfullname)
		fmt.Printf("pieces=%v \n", pieces)
		fmt.Printf("ticket=%v \n", ticketStr)

		return nil
	},
}
var preCommit2Cmd = &cli.Command{
	Name:  "precommit2",
	Usage: "Benchmark PreCommit2",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "Path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Value: "t01000",
			Usage: "miner address",
		},
		&cli.StringFlag{
			Name:  "sector-id",
			Value: "10",
			Usage: "sector number",
		},
	},
	Action: func(c *cli.Context) error {
		// storage-dir
		sdir, err := homedir.Expand(c.String("storage-dir"))
		if err != nil {
			return err
		}

		// sector id
		sectoridInt, err := units.RAMInBytes(c.String("sector-id"))
		if err != nil {
			return err
		}
		sectorid := abi.SectorNumber(sectoridInt)

		// miner address
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		var sectorfullname = "s-" + c.String("miner-id") + "-" + c.String("sector-id")

		// read p2in
		inb, err := ioutil.ReadFile("p2in.json")
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var p2in PreCommit2In
		if err := json.Unmarshal(inb, &p2in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,      //abi.ActorID(1000),
				Number: sectorid, //abi.SectorNumber(10),
			},
			ProofType: spt(abi.SectorSize(p2in.Size)),
		}

		pc1o := p2in.Data

		// config proofs
		sbfs := &basicfs.Provider{
			Root: sdir,
		}
		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		var sealTimings SealingResult
		pc2Start := time.Now()

		// start SealPreCommit2
		log.Infof("[%d] Running replication(2)...", 1)

		log.Infof("preCommit2Cmd sid=%v pc1o=%v", sid, pc1o)

		cids, err := sb.SealPreCommit2(context.TODO(), sid, pc1o)
		if err != nil {
			return xerrors.Errorf("commit: %w", err)
		}

		precommit2 := time.Now()

		c1in := Commit1In{
			Size:     p2in.Size,
			Unsealed: cids.Unsealed.String(),
			Sealed:   cids.Sealed.String(),
		}

		b, err := json.Marshal(&c1in)
		if err != nil {
			return err
		}

		if err := ioutil.WriteFile("c1in.json", b, 0664); err != nil {
			return err
		}

		sealTimings.PreCommit2 = precommit2.Sub(pc2Start)
		fmt.Printf("----\nresults (v28) (%d)\n", p2in.Size)
		fmt.Printf("seal: preCommit phase 2: %s (%s)\n", sealTimings.PreCommit2, bps(abi.SectorSize(p2in.Size), 1, sealTimings.PreCommit2))

		fmt.Printf("---- \n")
		fmt.Printf("%v \n", sectorfullname)
		fmt.Printf("c1in=%v \n", c1in)

		return nil
	},
}
var commit1Cmd = &cli.Command{
	Name:  "commit1",
	Usage: "Benchmark Commit1",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "Path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Value: "t01000",
			Usage: "miner address",
		},
		&cli.StringFlag{
			Name:  "sector-id",
			Value: "10",
			Usage: "sector number",
		},
		&cli.StringFlag{
			Name:  "seed",
			Value: "",
			Usage: "seed",
		},
		&cli.IntFlag{
			Name:  "seedH",
			Value: 101,
			Usage: "seed height",
		},
	},
	Action: func(c *cli.Context) error {
		// storage-dir
		sdir, err := homedir.Expand(c.String("storage-dir"))
		if err != nil {
			return err
		}

		// sector id
		sectoridInt, err := units.RAMInBytes(c.String("sector-id"))
		if err != nil {
			return err
		}
		sectorid := abi.SectorNumber(sectoridInt)

		// miner address
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		// seedH
		seedH := c.Int64("seedH")
		// seed
		var seedStr = c.String("seed")
		if len(seedStr) != 64 {
			//return xerrors.Errorf("seed len is fault.")
			seedStr = "a1fd6da64c25d4b53afa948ef0ef81aa48dbafbb561c63dfdbf12c954817fa05"
		}
		seedHex, err := hex.DecodeString(seedStr)
		if err != nil {
			return err
		}
		seed := lapi.SealSeed{
			Epoch: abi.ChainEpoch(seedH),
			Value: seedHex[:],
		}

		var sectorfullname = "s-" + c.String("miner-id") + "-" + c.String("sector-id")

		// read p1in
		inb, err := ioutil.ReadFile("p1in.json")
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var p1in PreCommit1In
		if err := json.Unmarshal(inb, &p1in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		_cid, err := cid.Decode(p1in.PieceCID)
		if err != nil {
			return err
		}

		pi := abi.PieceInfo{
			Size:     abi.PaddedPieceSize(p1in.Size),
			PieceCID: _cid,
		}

		// read p2in
		inb, err = ioutil.ReadFile("p2in.json")
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var p2in PreCommit2In
		if err := json.Unmarshal(inb, &p2in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		// read c1in
		inb, err = ioutil.ReadFile("c1in.json")
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var c1in Commit1In
		if err := json.Unmarshal(inb, &c1in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,      //abi.ActorID(1000),
				Number: sectorid, //abi.SectorNumber(10),
			},
			ProofType: spt(abi.SectorSize(p1in.Size)),
		}

		// config proofs
		sbfs := &basicfs.Provider{
			Root: sdir,
		}
		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		var sealTimings SealingResult
		commit1Start := time.Now()

		// start SealCommit1
		log.Infof("[%d] Generating PoRep for sector (1)", 1)
		pieces := []abi.PieceInfo{pi}

		Unsealed, err := cid.Decode(c1in.Unsealed)
		if err != nil {
			return err
		}
		Sealed, err := cid.Decode(c1in.Sealed)
		if err != nil {
			return err
		}

		cids := storage.SectorCids{
			Unsealed: Unsealed,
			Sealed:   Sealed,
		}

		ticket := p2in.Ticket

		log.Infof("commit1Cmd sid=%v ", sid)
		//log.Infof("commit1Cmd ticket-hex=%v", ticket)
		log.Infof("commit1Cmd seed=%v seed-hex=%v", seedStr, seed.Value)
		log.Infof("commit1Cmd pieces=%v", pieces)
		log.Infof("commit1Cmd cids=%v", cids)

		c1o, err := sb.SealCommit1(context.TODO(), sid, ticket, seed.Value, pieces, cids)
		if err != nil {
			return err
		}

		sealcommit1 := time.Now()

		c2in := Commit2In{
			SectorNum:  int64(sectorid),
			Phase1Out:  c1o,
			SectorSize: c1in.Size,
		}

		b, err := json.Marshal(&c2in)
		if err != nil {
			return err
		}

		if err := ioutil.WriteFile("c2in.json", b, 0664); err != nil {
			return err
		}

		sealTimings.Commit1 = sealcommit1.Sub(commit1Start)
		fmt.Printf("----\nresults (v28) (%d)\n", c1in.Size)
		fmt.Printf("seal: commit phase 1: %s (%s)\n", sealTimings.Commit1, bps(abi.SectorSize(c1in.Size), 1, sealTimings.Commit1))

		fmt.Printf("---- \n")
		fmt.Printf("%v \n", sectorfullname)
		fmt.Printf("pieces=%v \n", pieces)
		//fmt.Printf("ticket=%v \n", ticket)
		fmt.Printf("seed=%v \n", seedStr)
		fmt.Printf("cids=%v \n", cids)

		return nil
	},
}
var commit2Cmd = &cli.Command{
	Name:      "commit2",
	Usage:     "Benchmark Commit2",
	ArgsUsage: "[c2in.json]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "no-gpu",
			Usage: "disable gpu usage for the benchmark run",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Usage: "miner address",
			Value: "t01000",
		},
	},
	Action: func(c *cli.Context) error {
		if c.Bool("no-gpu") {
			err := os.Setenv("BELLMAN_NO_GPU", "1")
			if err != nil {
				return xerrors.Errorf("setting no-gpu flag: %w", err)
			}
		}

		// miner address
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		// read c2in
		// if !c.Args().Present() {
		// 	return xerrors.Errorf("Usage: lotus-bench commit2 [c2in.json]")
		// }
		var c2in_json = c.Args().First()
		if c2in_json == "" {
			c2in_json = "c2in.json"
		}
		inb, err := ioutil.ReadFile(c2in_json)
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var c2in Commit2In
		if err := json.Unmarshal(inb, &c2in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		if err := paramfetch.GetParams(lcli.ReqContext(c), build.ParametersJSON(), build.SrsJSON(), c2in.SectorSize); err != nil {
			return xerrors.Errorf("getting params: %w", err)
		}

		sb, err := ffiwrapper.New(nil)
		if err != nil {
			return err
		}

		log.Infof("[%d] Generating PoRep for sector (2)", 1)
		log.Infof("commit2Cmd c2in.SectorNum %v", c2in.SectorNum)
		fmt.Printf("----\nstart proof computation\n")

		ref := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,
				Number: abi.SectorNumber(c2in.SectorNum),
			},
			ProofType: spt(abi.SectorSize(c2in.SectorSize)),
		}

		var sealTimings SealingResult
		commit2Start := time.Now()

		proof, err := sb.SealCommit2(context.TODO(), ref, c2in.Phase1Out)
		if err != nil {
			return err
		}

		sealCommit2 := time.Now()

		fmt.Printf("proof: %x\n", proof)

		sealTimings.Commit2 = sealCommit2.Sub(commit2Start)
		fmt.Printf("----\nresults (v28) (%d)\n", c2in.SectorSize)
		fmt.Printf("seal: commit phase 2: %s (%s)\n", sealTimings.Commit2, bps(abi.SectorSize(c2in.SectorSize), 1, sealTimings.Commit2))

		return nil
	},
}

// sector-recovery  //powerd by https://www.pocyc.com
var recoveryCmd = &cli.Command{
	Name:  "recovery",
	Usage: "sector recovery tool",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "Path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "sector-size",
			Value: "2KiB",
			Usage: "size of the sectors in bytes, i.e. 32GiB",
		},
		&cli.StringFlag{
			Name:  "miner-id",
			Value: "t01000",
			Usage: "miner address",
		},
		&cli.StringFlag{
			Name:  "sector-id",
			Value: "10",
			Usage: "sector number",
		},
		&cli.StringFlag{
			Name:  "ticket",
			Value: "",
			Usage: "ticket",
		},
		&cli.BoolFlag{
			Name:  "clear",
			Usage: "clear cache/data-layer cache/tree-d cache/tree-c (default false)",
			Value: false,
		},
	},
	Action: func(c *cli.Context) error {
		// storage-dir
		sdir, err := homedir.Expand(c.String("storage-dir"))
		if err != nil {
			return err
		}
		err = os.MkdirAll(sdir, 0775) //nolint:gosec
		if err != nil {
			return xerrors.Errorf("creating sectorbuilder dir: %w", err)
		}
		// config proofs
		sbfs := &basicfs.Provider{
			Root: sdir,
		}
		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		// sector-size
		sectorSizeInt, err := units.RAMInBytes(c.String("sector-size"))
		if err != nil {
			return err
		}
		sectorSize := abi.SectorSize(sectorSizeInt)

		// sector-id
		sectoridInt, err := units.RAMInBytes(c.String("sector-id"))
		if err != nil {
			return err
		}
		sectorid := abi.SectorNumber(sectoridInt)

		// miner-id
		maddr, err := address.NewFromString(c.String("miner-id"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		var sectorfullname = "s-" + c.String("miner-id") + "-" + c.String("sector-id")

		// ticket
		var ticketStr = c.String("ticket")
		if len(ticketStr) != 64 {
			return xerrors.Errorf("ticket len is fault.")
		}
		ticketHex, err := hex.DecodeString(ticketStr)
		if err != nil {
			return err
		}
		ticket := abi.SealRandomness(ticketHex[:])

		// clear
		var clear_cache = c.Bool("clear")

		var sealTimings SealingResult
		start := time.Now()

		// start AddPiece
		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,
				Number: sectorid,
			},
			ProofType: spt(sectorSize),
		}

		log.Infof("[%d] Writing piece into sector...", 1)
		log.Infof("recovery addPieceCmd sid=%v", sid)

		pi, err := sb.AddPiece(context.TODO(), sid, nil, abi.PaddedPieceSize(sectorSize).Unpadded(), sealing.NewNullReader(abi.PaddedPieceSize(sectorSize).Unpadded()))
		if err != nil {
			return err
		}

		addpiece := time.Now()
		pc1Start := time.Now()

		pieces := []abi.PieceInfo{pi}
		// start PreCommit1
		log.Infof("[%d] Running replication(1)...", 1)

		log.Infof("recovery preCommit1Cmd sid=%v", sid)
		log.Infof("recovery preCommit1Cmd ticket=%v", ticketStr)
		log.Infof("recovery preCommit1Cmd pieces=%v", pieces)

		pc1o, err := sb.SealPreCommit1(context.TODO(), sid, ticket, pieces)

		if err != nil {
			return xerrors.Errorf("commit: %w", err)
		}

		precommit1 := time.Now()
		pc2Start := time.Now()

		// start SealPreCommit2
		log.Infof("[%d] Running replication(2)...", 1)

		log.Infof("recovery preCommit2Cmd sid=%v ", sid)
		log.Infof("recovery preCommit2Cmd pc1o=%v ", pc1o)
		cids, err := sb.SealPreCommit2(context.TODO(), sid, pc1o)
		if err != nil {
			return xerrors.Errorf("commit: %w", err)
		}

		precommit2 := time.Now()

		// clear_cache
		if !clear_cache {
			var fileunsealed = sdir + "/unsealed/" + sectorfullname
			if err := os.RemoveAll(fileunsealed); err != nil {
				return xerrors.Errorf("remove existing sector cache from %s (sector %d): %w", fileunsealed, sectoridInt, err)
			}
			_ = sb.FinalizeSector(context.TODO(), sid, nil)
		}

		sealTimings.AddPiece = addpiece.Sub(start)
		sealTimings.PreCommit1 = precommit1.Sub(pc1Start)
		sealTimings.PreCommit2 = precommit2.Sub(pc2Start)

		fmt.Printf("----\nresults (v28) (%d)\n", sectorSize)
		fmt.Printf("seal: addPiece: %s (%s)\n", sealTimings.AddPiece, bps(sectorSize, 1, sealTimings.AddPiece))
		fmt.Printf("seal: preCommit phase 1: %s (%s)\n", sealTimings.PreCommit1, bps(sectorSize, 1, sealTimings.PreCommit1))
		fmt.Printf("seal: preCommit phase 2: %s (%s)\n", sealTimings.PreCommit2, bps(sectorSize, 1, sealTimings.PreCommit2))

		fmt.Printf("----\n")
		fmt.Printf("%v \n", sectorfullname)
		fmt.Printf("recovery  Ticket=%v \n", ticketStr)
		fmt.Printf("recovery  CIDcommD=%v \n", cids.Unsealed.String())
		fmt.Printf("recovery  CIDcommR=%v \n", cids.Sealed.String())

		return nil
	},
}

var sealBenchCmd = &cli.Command{
	Name:  "sealing",
	Usage: "Benchmark seal and winning post and window post",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:  "storage-dir",
			Value: "~/.lotus-bench",
			Usage: "path to the storage directory that will store sectors long term",
		},
		&cli.StringFlag{
			Name:  "sector-size",
			Value: "512MiB",
			Usage: "size of the sectors in bytes, i.e. 32GiB",
		},
		&cli.BoolFlag{
			Name:  "no-gpu",
			Usage: "disable gpu usage for the benchmark run",
		},
		&cli.StringFlag{
			Name:  "miner-addr",
			Usage: "pass miner address (only necessary if using existing sectorbuilder)",
			Value: "t01000",
		},
		&cli.StringFlag{
			Name:  "benchmark-existing-sectorbuilder",
			Usage: "pass a directory to run post timings on an existing sectorbuilder",
		},
		&cli.BoolFlag{
			Name:  "json-out",
			Usage: "output results in json format",
		},
		&cli.BoolFlag{
			Name:  "skip-commit2",
			Usage: "skip the commit2 (snark) portion of the benchmark",
		},
		&cli.BoolFlag{
			Name:  "skip-unseal",
			Usage: "skip the unseal portion of the benchmark",
		},
		&cli.StringFlag{
			Name:  "ticket-preimage",
			Usage: "ticket random",
		},
		&cli.StringFlag{
			Name:  "save-commit2-input",
			Usage: "save commit2 input to a file",
		},
		&cli.IntFlag{
			Name:  "num-sectors",
			Usage: "select number of sectors to seal",
			Value: 1,
		},
		&cli.IntFlag{
			Name:  "parallel",
			Usage: "num run in parallel",
			Value: 1,
		},
	},
	Action: func(c *cli.Context) error {
		if c.Bool("no-gpu") {
			err := os.Setenv("BELLMAN_NO_GPU", "1")
			if err != nil {
				return xerrors.Errorf("setting no-gpu flag: %w", err)
			}
		}

		robench := c.String("benchmark-existing-sectorbuilder")

		var sbdir string

		if robench == "" {
			sdir, err := homedir.Expand(c.String("storage-dir"))
			if err != nil {
				return err
			}

			err = os.MkdirAll(sdir, 0775) //nolint:gosec
			if err != nil {
				return xerrors.Errorf("creating sectorbuilder dir: %w", err)
			}

			tsdir, err := ioutil.TempDir(sdir, "bench")
			if err != nil {
				return err
			}
			defer func() {
				if err := os.RemoveAll(tsdir); err != nil {
					log.Warn("remove all: ", err)
				}
			}()

			// TODO: pretty sure this isnt even needed?
			if err := os.MkdirAll(tsdir, 0775); err != nil {
				return err
			}

			sbdir = tsdir
		} else {
			exp, err := homedir.Expand(robench)
			if err != nil {
				return err
			}
			sbdir = exp
		}

		// miner address
		maddr, err := address.NewFromString(c.String("miner-addr"))
		if err != nil {
			return err
		}
		amid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}
		mid := abi.ActorID(amid)

		// sector size
		sectorSizeInt, err := units.RAMInBytes(c.String("sector-size"))
		if err != nil {
			return err
		}
		sectorSize := abi.SectorSize(sectorSizeInt)

		// Only fetch parameters if actually needed
		skipc2 := c.Bool("skip-commit2")
		if !skipc2 {
			if err := paramfetch.GetParams(lcli.ReqContext(c), build.ParametersJSON(), build.SrsJSON(), uint64(sectorSize)); err != nil {
				return xerrors.Errorf("getting params: %w", err)
			}
		}

		sbfs := &basicfs.Provider{
			Root: sbdir,
		}

		sb, err := ffiwrapper.New(sbfs)
		if err != nil {
			return err
		}

		sectorNumber := c.Int("num-sectors")

		var sealTimings []SealingResult
		var sealedSectors []saproof2.SectorInfo

		if robench == "" {
			var err error
			parCfg := ParCfg{
				PreCommit1: c.Int("parallel"),
				PreCommit2: 1,
				Commit:     1,
			}
			sealTimings, sealedSectors, err = runSeals(sb, sbfs, sectorNumber, parCfg, mid, sectorSize, []byte(c.String("ticket-preimage")), c.String("save-commit2-input"), skipc2, c.Bool("skip-unseal"))
			if err != nil {
				return xerrors.Errorf("failed to run seals: %w", err)
			}
		} else {
			// TODO: implement sbfs.List() and use that for all cases (preexisting sectorbuilder or not)

			// TODO: this assumes we only ever benchmark a preseal
			// sectorbuilder directory... we need a better way to handle
			// this in other cases

			fdata, err := ioutil.ReadFile(filepath.Join(sbdir, "pre-seal-"+maddr.String()+".json"))
			if err != nil {
				return err
			}

			var genmm map[string]genesis.Miner
			if err := json.Unmarshal(fdata, &genmm); err != nil {
				return err
			}

			genm, ok := genmm[maddr.String()]
			if !ok {
				return xerrors.Errorf("preseal file didnt have expected miner in it")
			}

			for _, s := range genm.Sectors {
				sealedSectors = append(sealedSectors, saproof2.SectorInfo{
					SealedCID:    s.CommR,
					SectorNumber: s.SectorID,
					SealProof:    s.ProofType,
				})
			}
		}

		bo := BenchResults{
			SectorSize:     sectorSize,
			SectorNumber:   sectorNumber,
			SealingResults: sealTimings,
		}
		if err := bo.SumSealingTime(); err != nil {
			return err
		}

		var challenge [32]byte
		rand.Read(challenge[:])

		beforePost := time.Now()

		if !skipc2 {
			log.Info("generating winning post candidates")
			wipt, err := spt(sectorSize).RegisteredWinningPoStProof()
			if err != nil {
				return err
			}

			fcandidates, err := ffiwrapper.ProofVerifier.GenerateWinningPoStSectorChallenge(context.TODO(), wipt, mid, challenge[:], uint64(len(sealedSectors)))
			if err != nil {
				return err
			}

			candidates := make([]saproof2.SectorInfo, len(fcandidates))
			for i, fcandidate := range fcandidates {
				candidates[i] = sealedSectors[fcandidate]
			}

			gencandidates := time.Now()

			log.Info("computing winning post snark (cold)")
			proof1, err := sb.GenerateWinningPoSt(context.TODO(), mid, candidates, challenge[:])
			if err != nil {
				return err
			}

			winningpost1 := time.Now()

			log.Info("computing winning post snark (hot)")
			proof2, err := sb.GenerateWinningPoSt(context.TODO(), mid, candidates, challenge[:])
			if err != nil {
				return err
			}

			winnningpost2 := time.Now()

			pvi1 := saproof2.WinningPoStVerifyInfo{
				Randomness:        abi.PoStRandomness(challenge[:]),
				Proofs:            proof1,
				ChallengedSectors: candidates,
				Prover:            mid,
			}
			ok, err := ffiwrapper.ProofVerifier.VerifyWinningPoSt(context.TODO(), pvi1)
			if err != nil {
				return err
			}
			if !ok {
				log.Error("post verification failed")
			}

			verifyWinningPost1 := time.Now()

			pvi2 := saproof2.WinningPoStVerifyInfo{
				Randomness:        abi.PoStRandomness(challenge[:]),
				Proofs:            proof2,
				ChallengedSectors: candidates,
				Prover:            mid,
			}

			ok, err = ffiwrapper.ProofVerifier.VerifyWinningPoSt(context.TODO(), pvi2)
			if err != nil {
				return err
			}
			if !ok {
				log.Error("post verification failed")
			}
			verifyWinningPost2 := time.Now()

			log.Info("computing window post snark (cold)")
			wproof1, _, err := sb.GenerateWindowPoSt(context.TODO(), mid, sealedSectors, challenge[:])
			if err != nil {
				return err
			}

			windowpost1 := time.Now()

			log.Info("computing window post snark (hot)")
			wproof2, _, err := sb.GenerateWindowPoSt(context.TODO(), mid, sealedSectors, challenge[:])
			if err != nil {
				return err
			}

			windowpost2 := time.Now()

			wpvi1 := saproof2.WindowPoStVerifyInfo{
				Randomness:        challenge[:],
				Proofs:            wproof1,
				ChallengedSectors: sealedSectors,
				Prover:            mid,
			}
			ok, err = ffiwrapper.ProofVerifier.VerifyWindowPoSt(context.TODO(), wpvi1)
			if err != nil {
				return err
			}
			if !ok {
				log.Error("window post verification failed")
			}

			verifyWindowpost1 := time.Now()

			wpvi2 := saproof2.WindowPoStVerifyInfo{
				Randomness:        challenge[:],
				Proofs:            wproof2,
				ChallengedSectors: sealedSectors,
				Prover:            mid,
			}
			ok, err = ffiwrapper.ProofVerifier.VerifyWindowPoSt(context.TODO(), wpvi2)
			if err != nil {
				return err
			}
			if !ok {
				log.Error("window post verification failed")
			}

			verifyWindowpost2 := time.Now()

			bo.PostGenerateCandidates = gencandidates.Sub(beforePost)
			bo.PostWinningProofCold = winningpost1.Sub(gencandidates)
			bo.PostWinningProofHot = winnningpost2.Sub(winningpost1)
			bo.VerifyWinningPostCold = verifyWinningPost1.Sub(winnningpost2)
			bo.VerifyWinningPostHot = verifyWinningPost2.Sub(verifyWinningPost1)

			bo.PostWindowProofCold = windowpost1.Sub(verifyWinningPost2)
			bo.PostWindowProofHot = windowpost2.Sub(windowpost1)
			bo.VerifyWindowPostCold = verifyWindowpost1.Sub(windowpost2)
			bo.VerifyWindowPostHot = verifyWindowpost2.Sub(verifyWindowpost1)
		}

		bo.EnvVar = make(map[string]string)
		for _, envKey := range []string{"BELLMAN_NO_GPU", "FIL_PROOFS_MAXIMIZE_CACHING", "FIL_PROOFS_USE_GPU_COLUMN_BUILDER",
			"FIL_PROOFS_USE_GPU_TREE_BUILDER", "FIL_PROOFS_USE_MULTICORE_SDR", "BELLMAN_CUSTOM_GPU"} {
			envValue, found := os.LookupEnv(envKey)
			if found {
				bo.EnvVar[envKey] = envValue
			}
		}

		if c.Bool("json-out") {
			data, err := json.MarshalIndent(bo, "", "  ")
			if err != nil {
				return err
			}

			fmt.Println(string(data))
		} else {
			fmt.Println("environment variable list:")
			for envKey, envValue := range bo.EnvVar {
				fmt.Printf("%s=%s\n", envKey, envValue)
			}
			fmt.Printf("----\nresults (v28) SectorSize:(%d), SectorNumber:(%d)\n", sectorSize, sectorNumber)
			if robench == "" {
				fmt.Printf("seal: addPiece: %s (%s)\n", bo.SealingSum.AddPiece, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.AddPiece))
				fmt.Printf("seal: preCommit phase 1: %s (%s)\n", bo.SealingSum.PreCommit1, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.PreCommit1))
				fmt.Printf("seal: preCommit phase 2: %s (%s)\n", bo.SealingSum.PreCommit2, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.PreCommit2))
				fmt.Printf("seal: commit phase 1: %s (%s)\n", bo.SealingSum.Commit1, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.Commit1))
				fmt.Printf("seal: commit phase 2: %s (%s)\n", bo.SealingSum.Commit2, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.Commit2))
				fmt.Printf("seal: verify: %s\n", bo.SealingSum.Verify)
				if !c.Bool("skip-unseal") {
					fmt.Printf("unseal: %s  (%s)\n", bo.SealingSum.Unseal, bps(bo.SectorSize, bo.SectorNumber, bo.SealingSum.Unseal))
				}
				fmt.Println("")
			}
			if !skipc2 {
				fmt.Printf("generate candidates: %s (%s)\n", bo.PostGenerateCandidates, bps(bo.SectorSize, len(bo.SealingResults), bo.PostGenerateCandidates))
				fmt.Printf("compute winning post proof (cold): %s\n", bo.PostWinningProofCold)
				fmt.Printf("compute winning post proof (hot): %s\n", bo.PostWinningProofHot)
				fmt.Printf("verify winning post proof (cold): %s\n", bo.VerifyWinningPostCold)
				fmt.Printf("verify winning post proof (hot): %s\n\n", bo.VerifyWinningPostHot)

				fmt.Printf("compute window post proof (cold): %s\n", bo.PostWindowProofCold)
				fmt.Printf("compute window post proof (hot): %s\n", bo.PostWindowProofHot)
				fmt.Printf("verify window post proof (cold): %s\n", bo.VerifyWindowPostCold)
				fmt.Printf("verify window post proof (hot): %s\n", bo.VerifyWindowPostHot)
			}
		}
		return nil
	},
}

type ParCfg struct {
	PreCommit1 int
	PreCommit2 int
	Commit     int
}

func runSeals(sb *ffiwrapper.Sealer, sbfs *basicfs.Provider, numSectors int, par ParCfg, mid abi.ActorID, sectorSize abi.SectorSize, ticketPreimage []byte, saveC2inp string, skipc2, skipunseal bool) ([]SealingResult, []saproof2.SectorInfo, error) {
	var pieces []abi.PieceInfo
	sealTimings := make([]SealingResult, numSectors)
	sealedSectors := make([]saproof2.SectorInfo, numSectors)

	preCommit2Sema := make(chan struct{}, par.PreCommit2)
	commitSema := make(chan struct{}, par.Commit)

	if numSectors%par.PreCommit1 != 0 {
		return nil, nil, fmt.Errorf("parallelism factor must cleanly divide numSectors")
	}
	for i := abi.SectorNumber(0); i < abi.SectorNumber(numSectors); i++ {
		sid := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  mid,
				Number: i,
			},
			ProofType: spt(sectorSize),
		}

		start := time.Now()
		log.Infof("[%d] Writing piece into sector...", i)

		r := rand.New(rand.NewSource(100 + int64(i)))

		pi, err := sb.AddPiece(context.TODO(), sid, nil, abi.PaddedPieceSize(sectorSize).Unpadded(), r)
		if err != nil {
			return nil, nil, err
		}

		pieces = append(pieces, pi)

		sealTimings[i].AddPiece = time.Since(start)
	}

	sectorsPerWorker := numSectors / par.PreCommit1

	errs := make(chan error, par.PreCommit1)
	for wid := 0; wid < par.PreCommit1; wid++ {
		go func(worker int) {
			sealerr := func() error {
				start := worker * sectorsPerWorker
				end := start + sectorsPerWorker
				for i := abi.SectorNumber(start); i < abi.SectorNumber(end); i++ {
					sid := storage.SectorRef{
						ID: abi.SectorID{
							Miner:  mid,
							Number: i,
						},
						ProofType: spt(sectorSize),
					}

					start := time.Now()

					trand := blake2b.Sum256(ticketPreimage)
					ticket := abi.SealRandomness(trand[:])

					log.Infof("[%d] Running replication(1)...", i)
					piece := []abi.PieceInfo{pieces[i]}
					pc1o, err := sb.SealPreCommit1(context.TODO(), sid, ticket, piece)
					if err != nil {
						return xerrors.Errorf("commit: %w", err)
					}

					precommit1 := time.Now()

					preCommit2Sema <- struct{}{}
					pc2Start := time.Now()
					log.Infof("[%d] Running replication(2)...", i)
					cids, err := sb.SealPreCommit2(context.TODO(), sid, pc1o)
					if err != nil {
						return xerrors.Errorf("commit: %w", err)
					}

					precommit2 := time.Now()
					<-preCommit2Sema

					sealedSectors[i] = saproof2.SectorInfo{
						SealProof:    sid.ProofType,
						SectorNumber: i,
						SealedCID:    cids.Sealed,
					}

					seed := lapi.SealSeed{
						Epoch: 101,
						Value: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 255},
					}

					commitSema <- struct{}{}
					commitStart := time.Now()
					log.Infof("[%d] Generating PoRep for sector (1)", i)
					c1o, err := sb.SealCommit1(context.TODO(), sid, ticket, seed.Value, piece, cids)
					if err != nil {
						return err
					}

					sealcommit1 := time.Now()

					log.Infof("[%d] Generating PoRep for sector (2)", i)

					if saveC2inp != "" {
						c2in := Commit2In{
							SectorNum:  int64(i),
							Phase1Out:  c1o,
							SectorSize: uint64(sectorSize),
						}

						b, err := json.Marshal(&c2in)
						if err != nil {
							return err
						}

						if err := ioutil.WriteFile(saveC2inp, b, 0664); err != nil {
							log.Warnf("%+v", err)
						}
					}

					var proof storage.Proof
					if !skipc2 {
						proof, err = sb.SealCommit2(context.TODO(), sid, c1o)
						if err != nil {
							return err
						}
					}

					sealcommit2 := time.Now()
					<-commitSema

					if !skipc2 {
						svi := saproof2.SealVerifyInfo{
							SectorID:              abi.SectorID{Miner: mid, Number: i},
							SealedCID:             cids.Sealed,
							SealProof:             sid.ProofType,
							Proof:                 proof,
							DealIDs:               nil,
							Randomness:            ticket,
							InteractiveRandomness: seed.Value,
							UnsealedCID:           cids.Unsealed,
						}

						ok, err := ffiwrapper.ProofVerifier.VerifySeal(svi)
						if err != nil {
							return err
						}
						if !ok {
							return xerrors.Errorf("porep proof for sector %d was invalid", i)
						}
					}

					verifySeal := time.Now()

					if !skipunseal {
						log.Infof("[%d] Unsealing sector", i)
						{
							p, done, err := sbfs.AcquireSector(context.TODO(), sid, storiface.FTUnsealed, storiface.FTNone, storiface.PathSealing)
							if err != nil {
								return xerrors.Errorf("acquire unsealed sector for removing: %w", err)
							}
							done()

							if err := os.Remove(p.Unsealed); err != nil {
								return xerrors.Errorf("removing unsealed sector: %w", err)
							}
						}

						err := sb.UnsealPiece(context.TODO(), sid, 0, abi.PaddedPieceSize(sectorSize).Unpadded(), ticket, cids.Unsealed)
						if err != nil {
							return err
						}
					}
					unseal := time.Now()

					sealTimings[i].PreCommit1 = precommit1.Sub(start)
					sealTimings[i].PreCommit2 = precommit2.Sub(pc2Start)
					sealTimings[i].Commit1 = sealcommit1.Sub(commitStart)
					sealTimings[i].Commit2 = sealcommit2.Sub(sealcommit1)
					sealTimings[i].Verify = verifySeal.Sub(sealcommit2)
					sealTimings[i].Unseal = unseal.Sub(verifySeal)
				}
				return nil
			}()
			if sealerr != nil {
				errs <- sealerr
				return
			}
			errs <- nil
		}(wid)
	}

	for i := 0; i < par.PreCommit1; i++ {
		err := <-errs
		if err != nil {
			return nil, nil, err
		}
	}

	return sealTimings, sealedSectors, nil
}

var proveCmd = &cli.Command{
	Name:      "prove",
	Usage:     "Benchmark a proof computation",
	ArgsUsage: "[input.json]",
	Flags: []cli.Flag{
		&cli.BoolFlag{
			Name:  "no-gpu",
			Usage: "disable gpu usage for the benchmark run",
		},
		&cli.StringFlag{
			Name:  "miner-addr",
			Usage: "pass miner address (only necessary if using existing sectorbuilder)",
			Value: "t01000",
		},
	},
	Action: func(c *cli.Context) error {
		if c.Bool("no-gpu") {
			err := os.Setenv("BELLMAN_NO_GPU", "1")
			if err != nil {
				return xerrors.Errorf("setting no-gpu flag: %w", err)
			}
		}

		if !c.Args().Present() {
			return xerrors.Errorf("Usage: lotus-bench prove [input.json]")
		}

		inb, err := ioutil.ReadFile(c.Args().First())
		if err != nil {
			return xerrors.Errorf("reading input file: %w", err)
		}

		var c2in Commit2In
		if err := json.Unmarshal(inb, &c2in); err != nil {
			return xerrors.Errorf("unmarshalling input file: %w", err)
		}

		if err := paramfetch.GetParams(lcli.ReqContext(c), build.ParametersJSON(), build.SrsJSON(), c2in.SectorSize); err != nil {
			return xerrors.Errorf("getting params: %w", err)
		}

		maddr, err := address.NewFromString(c.String("miner-addr"))
		if err != nil {
			return err
		}
		mid, err := address.IDFromAddress(maddr)
		if err != nil {
			return err
		}

		sb, err := ffiwrapper.New(nil)
		if err != nil {
			return err
		}

		ref := storage.SectorRef{
			ID: abi.SectorID{
				Miner:  abi.ActorID(mid),
				Number: abi.SectorNumber(c2in.SectorNum),
			},
			ProofType: spt(abi.SectorSize(c2in.SectorSize)),
		}

		fmt.Printf("----\nstart proof computation\n")
		start := time.Now()

		proof, err := sb.SealCommit2(context.TODO(), ref, c2in.Phase1Out)
		if err != nil {
			return err
		}

		sealCommit2 := time.Now()

		fmt.Printf("proof: %x\n", proof)

		fmt.Printf("----\nresults (v28) (%d)\n", c2in.SectorSize)
		dur := sealCommit2.Sub(start)

		fmt.Printf("seal: commit phase 2: %s (%s)\n", dur, bps(abi.SectorSize(c2in.SectorSize), 1, dur))
		return nil
	},
}

func bps(sectorSize abi.SectorSize, sectorNum int, d time.Duration) string {
	bdata := new(big.Int).SetUint64(uint64(sectorSize))
	bdata = bdata.Mul(bdata, big.NewInt(int64(sectorNum)))
	bdata = bdata.Mul(bdata, big.NewInt(time.Second.Nanoseconds()))
	bps := bdata.Div(bdata, big.NewInt(d.Nanoseconds()))
	return types.SizeStr(types.BigInt{Int: bps}) + "/s"
}

func spt(ssize abi.SectorSize) abi.RegisteredSealProof {
	spt, err := miner.SealProofTypeFromSectorSize(ssize, build.NewestNetworkVersion)
	if err != nil {
		panic(err)
	}

	return spt
}
