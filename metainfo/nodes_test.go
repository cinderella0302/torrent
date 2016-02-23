package metainfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFileNodesMatch(t *testing.T, file string, nodes []Node) {
	mi, err := LoadFromFile(file)
	require.NoError(t, err)
	assert.EqualValues(t, nodes, mi.Nodes)
}

func TestNodesListStrings(t *testing.T) {
	testFileNodesMatch(t, "testdata/trackerless.torrent", []Node{
		"udp://tracker.openbittorrent.com:80",
		"udp://tracker.openbittorrent.com:80",
	})
}

func TestNodesListPairsBEP5(t *testing.T) {
	testFileNodesMatch(t, "testdata/issue_65a.torrent", []Node{
		"185.34.3.132:5680",
		"185.34.3.103:12340",
		"94.209.253.165:47232",
		"78.46.103.11:34319",
		"195.154.162.70:55011",
		"185.34.3.137:3732",
	})
	testFileNodesMatch(t, "testdata/issue_65b.torrent", []Node{
		"95.211.203.130:6881",
		"84.72.116.169:6889",
		"204.83.98.77:7000",
		"101.187.175.163:19665",
		"37.187.118.32:6881",
		"83.128.223.71:23865",
	})
}
