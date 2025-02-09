package grid_tree

import "C"
import (
	"github.com/mfbonfigli/gocesiumtiler/internal/converters"
	"github.com/mfbonfigli/gocesiumtiler/internal/data"
	"github.com/mfbonfigli/gocesiumtiler/internal/geometry"
	"github.com/mfbonfigli/gocesiumtiler/internal/octree"
	"math"
	"sync"
	"sync/atomic"
)

// Models a node of the octree, which can either be a leaf (a node without children nodes) or not.
// Each Node can contain up to eight children nodes. The node uses a grid algorithm to decide which points to store.
// It divides its bounding box in gridCells and only stores points retained by these cells, propagating the ones rejected
// by the cells to its children which will have smaller cells.
type GridNode struct {
	root                bool
	parent              octree.INode
	boundingBox         *geometry.BoundingBox
	children            [8]octree.INode
	cells               map[gridIndex]*gridCell
	points              []*data.Point
	cellSize            float64
	minCellSize         float64
	totalNumberOfPoints int64
	numberOfPoints      int32
	leaf                int32
	initialized         bool
	rootGeometricError	float64
	sync.RWMutex
}

// Instantiates a new GridNode
func NewGridNode(parent octree.INode, boundingBox *geometry.BoundingBox, maxCellSize float64, minCellSize float64, root bool, rootGeometricError float64) octree.INode {
	node := GridNode{
		parent:              parent,						   // the parent node
		root:                root,                             // if the node is the tree root
		boundingBox:         boundingBox,                      // bounding box of the node
		cellSize:            maxCellSize,                      // max size setting to use for gridCells
		minCellSize:         minCellSize,                      // min size setting to use for gridCells
		points:              make([]*data.Point, 0),           // slice keeping references to points stored in the gridCells
		cells:               make(map[gridIndex]*gridCell, 0), // gridCells that subdivide this node bounding box
		totalNumberOfPoints: 0,                                // total number of points stored in this node and its children
		numberOfPoints:      0,                                // number of points stored in this node (children excluded)
		leaf:                1,                                // 1 if is a leaf, 0 otherwise
		initialized:         false,                            // flag to see if the node has been initialized
		rootGeometricError:  rootGeometricError,
	}

	return &node
}

// Adds a Point to the GridNode and propagates the point eventually pushed out to the appropriate children
func (n *GridNode) AddDataPoint(point *data.Point) {
	if point == nil {
		return
	}

	if n.isEmpty() {
		n.initializeChildren()
	}

	pushedOutPoint := n.pushPointToCell(point)

	if pushedOutPoint != nil {
		n.addPointToChildren(pushedOutPoint)
	} else {
		// if no point was rejected then the number of points stored is increased by 1
		atomic.AddInt32(&n.numberOfPoints, 1)
	}

	// in any case the total number of points stored by the n or its children increases by one
	atomic.AddInt64(&n.totalNumberOfPoints, 1)
}

func (n *GridNode) GetInternalSrid() int {
	return internalCoordinateEpsgCode
}

func (n *GridNode) GetBoundingBoxRegion(converter converters.CoordinateConverter) (*geometry.BoundingBox, error) {
	reg, err := converter.Convert2DBoundingboxToWGS84Region(n.boundingBox, n.GetInternalSrid())

	if err != nil {
		return nil, err
	}

	return reg, nil
}

func (n *GridNode) GetBoundingBox() *geometry.BoundingBox {
	return n.boundingBox
}

func (n *GridNode) GetChildren() [8]octree.INode {
	return n.children
}

func (n *GridNode) GetPoints() []*data.Point {
	return n.points
}

func (n *GridNode) TotalNumberOfPoints() int64 {
	return n.totalNumberOfPoints
}

func (n *GridNode) NumberOfPoints() int32 {
	return n.numberOfPoints
}

func (n *GridNode) IsLeaf() bool {
	return atomic.LoadInt32(&n.leaf) == 1
}

func (n *GridNode) IsInitialized() bool {
	return n.initialized
}

func (n *GridNode) IsRoot() bool {
	return n.root
}

// Computes the geometric error for the given GridNode
func (n *GridNode) ComputeGeometricError() float64 {
	// geometric error is estimated as the maximum possible distance between two points lying in the cell
	if n.IsRoot() {
		return n.cellSize * math.Sqrt(3) * 2 * n.rootGeometricError
	}
	return n.cellSize * math.Sqrt(3) * 2
}

// Returns the index of the octant that contains the given Point within this boundingBox
func getOctantFromElement(element *data.Point, bbox *geometry.BoundingBox) uint8 {
	var result uint8 = 0
	if float64(element.X) > bbox.Xmid {
		result += 1
	}
	if float64(element.Y) > bbox.Ymid {
		result += 2
	}
	if float64(element.Z) > bbox.Zmid {
		result += 4
	}
	return result
}

// loads the points stored in the grid cells into the slice data structure
// and recursively builds the points of its children.
// sets the slice reference to nil to allow GC to happen as the cells won't be used anymore
func (n *GridNode) BuildPoints() {
	var points []*data.Point
	for _, cell := range n.cells {
		points = append(points, cell.points...)
	}
	n.points = points
	n.cells = nil

	for _, child := range n.children {
		if child != nil {
			child.(*GridNode).BuildPoints()
		}
	}
}

func (n *GridNode) GetParent() octree.INode {
	return n.parent
}

// gets the grid cell where the given point falls into, eventually creating it if it does not exist
func (n *GridNode) getPointGridCell(point *data.Point) *gridCell {
	index := *n.getPointGridCellIndex(point)

	n.RLock()
	cell := n.cells[index]
	n.RUnlock()

	if cell == nil {
		return n.initializeGridCell(&index)
	}

	return cell
}

// returns the index of the cell where the input point is falling in
func (n *GridNode) getPointGridCellIndex(point *data.Point) *gridIndex {
	return &gridIndex{
		getDimensionIndex(point.X, n.cellSize),
		getDimensionIndex(point.Y, n.cellSize),
		getDimensionIndex(point.Z, n.cellSize),
	}
}

func (n *GridNode) initializeGridCell(index *gridIndex) *gridCell {
	n.Lock()

	out := n.cells[*index]
	if out == nil {
		out = &gridCell{
			index:         *index,
			size:          n.cellSize,
			sizeThreshold: n.minCellSize,
		}
		n.cells[*index] = out
	}

	n.Unlock()

	return out
}

// atomically checks if the node is empty
func (n *GridNode) isEmpty() bool {
	return atomic.LoadInt32(&n.numberOfPoints) == 0
}

// pushes a point to its gridcell and returns the point eventually pushed out
func (n *GridNode) pushPointToCell(point *data.Point) *data.Point {
	return n.getPointGridCell(point).pushPoint(point)
}

// add a point to the node children and clears the leaf flag from this node
func (n *GridNode) addPointToChildren(point *data.Point) {
	n.children[getOctantFromElement(point, n.boundingBox)].AddDataPoint(point)
	n.clearLeafFlag()
}

// sets the leaf flag to 0 atomically
func (n *GridNode) clearLeafFlag() {
	atomic.StoreInt32(&n.leaf, 0)
}

// initializes the children to new empty nodes
func (n *GridNode) initializeChildren() {
	n.Lock()
	for i := uint8(0); i < 8; i++ {
		if n.children[i] == nil {
			n.children[i] = NewGridNode(n, getOctantBoundingBox(&i, n.boundingBox), n.cellSize/2.0, n.minCellSize, false, n.rootGeometricError)
		}
	}
	n.initialized = true
	n.Unlock()
}

// Returns a bounding box from the given box and the given octant index
func getOctantBoundingBox(octant *uint8, bbox *geometry.BoundingBox) *geometry.BoundingBox {
	return geometry.NewBoundingBoxFromParent(bbox, octant)
}
