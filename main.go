package main

//github.com/pkg/sftp
import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

var (
	host       = "10.11.99.1:22"
	password   = "hF2Uu8EReA"
	defaultDir = ".local/share/remarkable/xochitl"
)

type rmNode struct {
	fs.Inode
	sc               *sftp.Client
	metadata         RemarkableMeta
	filenameToIno    map[string]uint64 // should make this a pointer for performance reasons
	visibleNameToIno map[string]uint64
	inoToFilename    map[uint64]string
	mu               sync.Mutex
	data             []byte
	fileSize         int64
}

type RemarkableMeta struct {
	Deleted          bool   `json:"deleted"`
	LastModified     string `json:"lastModified"`
	DocType          string `json:"type"`
	Parent           string `json:"parent"`
	VisibleName      string `json:"visibleName"`
	LastOpenedPage   int    `json:"lastOpenedPage"`
	Metadatamodified bool   `json:"metadatamodified"`
	Modified         bool   `json:"modified"`
	Pinned           bool   `json:"pinned"`
	Synced           bool   `json:"synced"`
	Version          int    `json:"version"`
}

func metadataTypeToMode(docType string) uint32 {
	if docType == "CollectionType" {
		return fuse.S_IFDIR
	}

	return fuse.S_IFREG
}

func getVisibleName(meta RemarkableMeta) string {
	var extension string

	if meta.DocType == "DocumentType" {
		extension = ".pdf"
	} else {
		extension = ""
	}

	return meta.VisibleName + extension
}

func parseMetadataFromFile(path string, sc *sftp.Client) (*RemarkableMeta, syscall.Errno) {
	openedFile, err := sc.Open(path)

	defer openedFile.Close()
	if err != nil {
		return nil, syscall.ENOENT
	}
	byteData, err := ioutil.ReadAll(openedFile)

	if err != nil {
		// EIO = ERROR I/O. Maybe other error? Ask
		return nil, syscall.EIO
	}

	var rmMeta RemarkableMeta

	json.Unmarshal(byteData, &rmMeta)

	openedFile.Close()
	return &rmMeta, 0
}

func parseFile(path string, sc *sftp.Client) ([]byte, syscall.Errno) {
	openedFile, err := sc.Open(path)

	defer openedFile.Close()
	if err != nil {
		return nil, syscall.ENOENT
	}
	byteData, err := ioutil.ReadAll(openedFile)

	if err != nil {
		// EIO = ERROR I/O. Maybe other error? Ask
		return nil, syscall.EIO
	}

	return byteData, 0
}

var _ = (fs.NodeOpener)((*rmNode)(nil))

func (n *rmNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.data == nil {
		filename := n.inoToFilename[n.visibleNameToIno[getVisibleName(n.metadata)]]

		d, err := n.sc.Open(defaultDir + "/" + filename + ".pdf")
		if err != nil {

			return nil, 0, syscall.EIO
		}

		content, err := ioutil.ReadAll(d)
		if err != nil {
			return nil, 0, syscall.EIO
		}
		n.data = content
		d.Close()
	}

	/// file should be immutabl,e so no file handle
	// TODO: also set permissions to make file immutable

	return nil, fuse.FOPEN_KEEP_CACHE, fs.OK
}

func (n *rmNode) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	end := int(off) + len(dest)
	if end > len(n.data) {
		end = len(n.data)
	}

	return fuse.ReadResultData(n.data[off:end]), fs.OK
}

var _ = (fs.NodeReaddirer)((*rmNode)(nil))

func (n *rmNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	files, err := n.sc.ReadDir(defaultDir)

	if err != nil {
		return nil, syscall.ENOENT
	}

	dirEntries := make([]fuse.DirEntry, 0, len(files))

	for _, file := range files {
		filenameArray := strings.Split(file.Name(), ".")

		if len(filenameArray) == 2 && filenameArray[1] == "metadata" {
			rmMeta, err := parseMetadataFromFile(defaultDir+"/"+file.Name(), n.sc)

			if err != 0 {
				return nil, err
			}

			filename := filenameArray[0]

			if _, ok := n.filenameToIno[filename]; ok == false {
				n.filenameToIno[filename] = uint64(len(n.filenameToIno) + 25)
				n.visibleNameToIno[getVisibleName(*rmMeta)] = n.filenameToIno[filename]
				n.inoToFilename[n.filenameToIno[filename]] = filename
			}

			if (len(n.metadata.VisibleName) == 0 && rmMeta.Parent == "") ||
				(n.inoToFilename[n.visibleNameToIno[getVisibleName(n.metadata)]] == rmMeta.Parent) {
				d := fuse.DirEntry{
					Name: getVisibleName(*rmMeta),
					Ino:  n.filenameToIno[filename],
					Mode: metadataTypeToMode(rmMeta.DocType),
				}

				dirEntries = append(dirEntries, d)
			}

		}
	}

	return fs.NewListDirStream(dirEntries), 0
}

var _ = (fs.NodeGetattrer)((*rmNode)(nil))

func (n *rmNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Size = uint64(n.fileSize)
	if len(n.metadata.LastModified) > 0 {
		time, _ := strconv.Atoi(n.metadata.LastModified[:len(n.metadata.LastModified)-3]) // why the fuck is year 5000?
		out.Mtime = uint64(time)
	}

	return 0
}

var _ = (fs.NodeLookuper)((*rmNode)(nil))

func (n *rmNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	id, ok := n.visibleNameToIno[name]

	if ok == false {
		return nil, syscall.ENOENT
	}

	// StableAttr holds immutable attributes of a object in the filesystem. (docs)
	rmMeta, err := parseMetadataFromFile(defaultDir+"/"+n.inoToFilename[id]+".metadata", n.sc)
	fileInfo, statError := n.sc.Stat(defaultDir + "/" + n.inoToFilename[id] + ".pdf")

	stable := fs.StableAttr{
		Mode: metadataTypeToMode(rmMeta.DocType),
		Ino:  uint64(id),
	}

	var fileSize int64

	if statError != nil {
		fileSize = 0
	} else {
		fileSize = fileInfo.Size()
	}

	if err != 0 {
		return nil, syscall.ENOENT
	}

	operations := &rmNode{
		filenameToIno:    n.filenameToIno,
		sc:               n.sc,
		metadata:         *rmMeta,
		fileSize:         fileSize,
		visibleNameToIno: n.visibleNameToIno,
		inoToFilename:    n.inoToFilename,
	}

	child := n.NewInode(ctx, operations, stable)
	return child, 0

}

var _ = (fs.NodeSetattrer)((*rmNode)(nil))

func (n *rmNode) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	fmt.Print("Setatter")
	out.Size = 10000
	return 0
}

var _ = (fs.NodeWriter)((*rmNode)(nil))

func (n *rmNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	fmt.Println("Write stuffz")
	n.mu.Lock()
	defer n.mu.Unlock()

	end := int64(len(data)) + off

	// resize n.data
	if int64(len(n.data)) < end {
		d := make([]byte, end)
		copy(d, n.data)
		n.data = d
	}

	copy(n.data[off:off+int64(len(data))], data)

	// n.metadata.DocType = "DocumentType"
	// n.metadata.Deleted = false
	// n.metadata.LastModified = strconv.FormatInt(time.Now().UnixNano(), 10)
	// n.metadata.Parent = ""
	// n.metadata.VisibleName = "UHHHH?"
	// n.metadata.LastOpenedPage = 0
	// n.metadata.Metadatamodified = true
	// n.metadata.Modified = true
	// n.metadata.Pinned = false
	// n.metadata.Synced = false
	// n.metadata.Version = 1

	return uint32(len(n.data)), 0
}

var _ = (fs.NodeFlusher)((*rmNode)(nil))

func (n *rmNode) Flush(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	fmt.Println("FLUSH", fh, n.metadata)
	return 0
}

// Access reports whether a directory can be accessed by the caller.
func (n *rmNode) Access(ctx context.Context, mask uint32) syscall.Errno {
	// TODO: parse the mask and return a more correct value instead of always
	// granting permission.
	return syscall.F_OK
}

func main() {
	// read arguments - namely the mountpoint
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Print("You need to pass the mountpoint as an arg")
		panic("You need to pass the mountpoint as an arg")
	}

	mountpoint := flag.Arg(0)

	sshConfig := &ssh.ClientConfig{
		User:            "root",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Auth: []ssh.AuthMethod{
			ssh.Password(password)},
	}

	connection, err := ssh.Dial("tcp", host, sshConfig)

	if err != nil {
		fmt.Errorf("Failed to dial")
		panic(err)
	}

	fmt.Print("OK")
	defer connection.Close()

	sc, err := sftp.NewClient(connection)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to start SFTP subsystem: %v\n", err)
		os.Exit(1)
	}
	defer sc.Close()

	filenameToIno := make(map[string]uint64)
	visibleNameToIno := make(map[string]uint64)
	inoToFilename := make(map[uint64]string)

	root := &rmNode{sc: sc, filenameToIno: filenameToIno, visibleNameToIno: visibleNameToIno, inoToFilename: inoToFilename}

	server, err := fs.Mount(mountpoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			// Set to true to see how the file system works.
			Debug: false,
		}})

	if err != nil {
		panic(err)
	}

	server.Wait()
	connection.Close()
	sc.Close()
}
