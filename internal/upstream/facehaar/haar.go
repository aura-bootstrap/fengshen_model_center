package facehaar

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"
)

const (
	haarScaleFactor  = 1.1
	haarMinNeighbors = 3
	maxDetectionSize = 640
)

type haarCascade struct {
	width    int
	height   int
	stages   []haarStage
	features []haarFeature
}

type haarStage struct {
	threshold   float64
	classifiers []haarClassifier
}

type haarClassifier struct {
	feature   int
	threshold float64
	left      float64
	right     float64
}

type haarFeature struct {
	rects []haarRect
}

type haarRect struct {
	x      int
	y      int
	width  int
	height int
	weight float64
}

type cascadeDocument struct {
	Cascade cascadeXML `xml:"cascade"`
}

type cascadeXML struct {
	Width    int         `xml:"width"`
	Height   int         `xml:"height"`
	Stages   stagesXML   `xml:"stages"`
	Features featuresXML `xml:"features"`
}

type stagesXML struct {
	Items []stageXML `xml:"_"`
}

type stageXML struct {
	Threshold float64 `xml:"stageThreshold"`
	Weak      weakXML `xml:"weakClassifiers"`
}

type weakXML struct {
	Items []classifierXML `xml:"_"`
}

type classifierXML struct {
	Nodes  string `xml:"internalNodes"`
	Leaves string `xml:"leafValues"`
}

type featuresXML struct {
	Items []featureXML `xml:"_"`
}

type featureXML struct {
	Rects  rectsXML `xml:"rects"`
	Tilted bool     `xml:"tilted"`
}

type rectsXML struct {
	Items []string `xml:"_"`
}

func parseHaarCascade(data []byte) (*haarCascade, error) {
	var document cascadeDocument
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	source := document.Cascade
	if source.Width <= 0 || source.Height <= 0 || len(source.Stages.Items) == 0 || len(source.Features.Items) == 0 {
		return nil, errors.New("invalid OpenCV Haar cascade dimensions or stages")
	}
	cascade := &haarCascade{width: source.Width, height: source.Height}
	for _, sourceFeature := range source.Features.Items {
		if sourceFeature.Tilted || len(sourceFeature.Rects.Items) == 0 {
			return nil, errors.New("unsupported OpenCV Haar feature")
		}
		feature := haarFeature{}
		for _, rawRect := range sourceFeature.Rects.Items {
			values, err := cascadeNumbers(rawRect, 5)
			if err != nil {
				return nil, err
			}
			rect := haarRect{
				x: int(values[0]), y: int(values[1]), width: int(values[2]), height: int(values[3]), weight: values[4],
			}
			if rect.x < 0 || rect.y < 0 || rect.width <= 0 || rect.height <= 0 ||
				rect.x+rect.width > cascade.width || rect.y+rect.height > cascade.height || rect.weight == 0 {
				return nil, errors.New("invalid OpenCV Haar feature rectangle")
			}
			feature.rects = append(feature.rects, rect)
		}
		cascade.features = append(cascade.features, feature)
	}
	for _, sourceStage := range source.Stages.Items {
		if len(sourceStage.Weak.Items) == 0 || math.IsNaN(sourceStage.Threshold) || math.IsInf(sourceStage.Threshold, 0) {
			return nil, errors.New("invalid OpenCV Haar stage")
		}
		stage := haarStage{threshold: sourceStage.Threshold}
		for _, sourceClassifier := range sourceStage.Weak.Items {
			nodes, err := cascadeNumbers(sourceClassifier.Nodes, 4)
			if err != nil {
				return nil, err
			}
			leaves, err := cascadeNumbers(sourceClassifier.Leaves, 2)
			if err != nil {
				return nil, err
			}
			feature := int(nodes[2])
			if nodes[0] != 0 || nodes[1] != -1 || nodes[2] < 0 || feature >= len(cascade.features) ||
				!finite64(nodes[3]) || !finite64(leaves[0]) || !finite64(leaves[1]) {
				return nil, errors.New("unsupported OpenCV Haar decision tree")
			}
			stage.classifiers = append(stage.classifiers, haarClassifier{
				feature: feature, threshold: nodes[3], left: leaves[0], right: leaves[1],
			})
		}
		cascade.stages = append(cascade.stages, stage)
	}
	return cascade, nil
}

func cascadeNumbers(raw string, expected int) ([]float64, error) {
	fields := strings.Fields(raw)
	if len(fields) != expected {
		return nil, fmt.Errorf("OpenCV Haar value count = %d, want %d", len(fields), expected)
	}
	values := make([]float64, len(fields))
	for index, field := range fields {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil || !finite64(value) {
			return nil, errors.New("invalid OpenCV Haar numeric value")
		}
		values[index] = value
	}
	return values, nil
}

func finite64(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

type integralImage struct {
	width  int
	stride int
	sum    []int64
	square []int64
}

func newIntegralImage(gray *image.Gray) *integralImage {
	width := gray.Bounds().Dx()
	height := gray.Bounds().Dy()
	integral := &integralImage{
		width: width, stride: width + 1,
		sum: make([]int64, (width+1)*(height+1)), square: make([]int64, (width+1)*(height+1)),
	}
	for y := 0; y < height; y++ {
		var rowSum, rowSquare int64
		for x := 0; x < width; x++ {
			value := int64(gray.GrayAt(gray.Bounds().Min.X+x, gray.Bounds().Min.Y+y).Y)
			rowSum += value
			rowSquare += value * value
			index := (y+1)*integral.stride + x + 1
			integral.sum[index] = integral.sum[index-integral.stride] + rowSum
			integral.square[index] = integral.square[index-integral.stride] + rowSquare
		}
	}
	return integral
}

func (i *integralImage) rectSum(x, y, width, height int) int64 {
	left := y*i.stride + x
	right := left + width
	bottomLeft := left + height*i.stride
	bottomRight := right + height*i.stride
	return i.sum[bottomRight] - i.sum[bottomLeft] - i.sum[right] + i.sum[left]
}

func (i *integralImage) rectSquare(x, y, width, height int) int64 {
	left := y*i.stride + x
	right := left + width
	bottomLeft := left + height*i.stride
	bottomRight := right + height*i.stride
	return i.square[bottomRight] - i.square[bottomLeft] - i.square[right] + i.square[left]
}

type haarCandidate struct {
	x      int
	y      int
	width  int
	height int
}

type haarCluster struct {
	x      int
	y      int
	width  int
	height int
	count  int
}

func (c *haarCascade) detect(ctx context.Context, gray *image.Gray) (bool, error) {
	if c == nil || gray == nil || gray.Bounds().Dx() < c.width || gray.Bounds().Dy() < c.height {
		return false, nil
	}
	integral := newIntegralImage(gray)
	var candidates []haarCandidate
	for scale := 1.0; ; scale *= haarScaleFactor {
		width := int(math.Round(float64(c.width) * scale))
		height := int(math.Round(float64(c.height) * scale))
		if width > gray.Bounds().Dx() || height > gray.Bounds().Dy() {
			break
		}
		step := max(1, int(math.Round(scale)))
		for y := 0; y+height <= gray.Bounds().Dy(); y += step {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			for x := 0; x+width <= gray.Bounds().Dx(); x += step {
				if c.pass(integral, x, y, width, height, scale) {
					candidates = append(candidates, haarCandidate{x: x, y: y, width: width, height: height})
				}
			}
		}
	}
	return groupHaarCandidates(ctx, candidates)
}

func (c *haarCascade) pass(integral *integralImage, x, y, width, height int, scale float64) bool {
	inverseArea := 1.0 / float64(width*height)
	mean := float64(integral.rectSum(x, y, width, height)) * inverseArea
	variance := float64(integral.rectSquare(x, y, width, height))*inverseArea - mean*mean
	standardDeviation := 1.0
	if variance > 1 {
		standardDeviation = math.Sqrt(variance)
	}
	for _, stage := range c.stages {
		stageSum := 0.0
		for _, classifier := range stage.classifiers {
			featureValue := 0.0
			for _, rect := range c.features[classifier.feature].rects {
				rectX := x + int(math.Round(float64(rect.x)*scale))
				rectY := y + int(math.Round(float64(rect.y)*scale))
				rectRight := x + int(math.Round(float64(rect.x+rect.width)*scale))
				rectBottom := y + int(math.Round(float64(rect.y+rect.height)*scale))
				featureValue += rect.weight * float64(integral.rectSum(rectX, rectY, rectRight-rectX, rectBottom-rectY))
			}
			featureValue *= inverseArea
			if featureValue < classifier.threshold*standardDeviation {
				stageSum += classifier.left
			} else {
				stageSum += classifier.right
			}
		}
		if stageSum < stage.threshold {
			return false
		}
	}
	return true
}

func groupHaarCandidates(ctx context.Context, candidates []haarCandidate) (bool, error) {
	clusters := make([]haarCluster, 0, len(candidates))
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		matched := false
		for index := range clusters {
			cluster := &clusters[index]
			if similarHaarRect(candidate, *cluster) {
				cluster.x += candidate.x
				cluster.y += candidate.y
				cluster.width += candidate.width
				cluster.height += candidate.height
				cluster.count++
				matched = true
				break
			}
		}
		if !matched {
			clusters = append(clusters, haarCluster{
				x: candidate.x, y: candidate.y, width: candidate.width, height: candidate.height, count: 1,
			})
		}
	}
	for _, cluster := range clusters {
		if cluster.count >= haarMinNeighbors {
			return true, nil
		}
	}
	return false, nil
}

func similarHaarRect(left haarCandidate, right haarCluster) bool {
	deltaWidth := 0.2 * float64(left.width+right.width/right.count) / 2
	deltaHeight := 0.2 * float64(left.height+right.height/right.count) / 2
	return math.Abs(float64(left.x-right.x/right.count)) <= deltaWidth &&
		math.Abs(float64(left.y-right.y/right.count)) <= deltaHeight &&
		math.Abs(float64(left.width-right.width/right.count)) <= deltaWidth &&
		math.Abs(float64(left.height-right.height/right.count)) <= deltaHeight
}

func detectionGray(source image.Image) *image.Gray {
	bounds := source.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	scale := 1.0
	if max(width, height) > maxDetectionSize {
		scale = float64(maxDetectionSize) / float64(max(width, height))
	}
	targetWidth := max(1, int(math.Round(float64(width)*scale)))
	targetHeight := max(1, int(math.Round(float64(height)*scale)))
	gray := image.NewGray(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := bounds.Min.Y + min(height-1, int(float64(y)/scale))
		for x := 0; x < targetWidth; x++ {
			sourceX := bounds.Min.X + min(width-1, int(float64(x)/scale))
			gray.SetGray(x, y, color.GrayModel.Convert(source.At(sourceX, sourceY)).(color.Gray))
		}
	}
	return gray
}

func flipGray(source *image.Gray) *image.Gray {
	flipped := image.NewGray(source.Bounds())
	for y := 0; y < source.Bounds().Dy(); y++ {
		for x := 0; x < source.Bounds().Dx(); x++ {
			flipped.SetGray(source.Bounds().Dx()-1-x, y, source.GrayAt(x, y))
		}
	}
	return flipped
}
