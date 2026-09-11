package ml

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log"
	"math"
	"os"

	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/image/draw"
)

const (
	// ArcFace ResNet50 expects 112x112 input
	arcfaceInputWidth  = 112
	arcfaceInputHeight = 112
	// 3 channels (RGB)
	arcfaceChannels = 3
	// ArcFace output: 512-dimensional embedding
	arcfaceEmbeddingSize = 512
)

// ONNXProvider implements FaceVerificationProvider using a local ONNX model (ArcFace).
// It compares two face images by extracting embeddings and computing cosine similarity.
type ONNXProvider struct {
	modelPath string
}

// NewONNXProvider initializes the ONNX provider.
// modelPath: path to the .onnx model file (e.g. ./models/arcface_resnet50.onnx)
// libPath:   path to the native libonnxruntime shared library (.so or .dylib)
func NewONNXProvider(modelPath, libPath string) (*ONNXProvider, error) {
	if _, err := os.Stat(modelPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("ONNX model file not found at %q: place arcface_resnet50.onnx there", modelPath)
	}
	if libPath != "" {
		if _, err := os.Stat(libPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("ONNX Runtime shared library not found at %q: download from github.com/microsoft/onnxruntime/releases", libPath)
		}
		ort.SetSharedLibraryPath(libPath)
	}

	if err := ort.InitializeEnvironment(); err != nil {
		return nil, fmt.Errorf("failed to initialize ONNX Runtime environment: %w", err)
	}

	return &ONNXProvider{modelPath: modelPath}, nil
}

// CompareFaces compares two face images (JPEG or PNG bytes) and returns
// a cosine similarity score between 0.0 and 1.0.
func (p *ONNXProvider) CompareFaces(ctx context.Context, face1, face2 []byte) (float64, error) {
	emb1, err := p.extractEmbedding(ctx, face1)
	if err != nil {
		return 0, fmt.Errorf("embedding face1: %w", err)
	}

	emb2, err := p.extractEmbedding(ctx, face2)
	if err != nil {
		return 0, fmt.Errorf("embedding face2: %w", err)
	}

	score := cosineSimilarity(emb1, emb2)
	// Clamp to [0, 1] — cosine can technically go slightly below 0
	if score < 0 {
		score = 0
	}

	return score, nil
}

// Close gracefully shuts down the ONNX Runtime environment
// to prevent crash-on-exit or leaked resources in the native C lib.
func (p *ONNXProvider) Close() error {
	if ort.IsInitialized() {
		return ort.DestroyEnvironment()
	}
	return nil
}

// extractEmbedding decodes an image, resizes it to 112x112, and runs
// it through the ArcFace ONNX model to get a 512-dim face embedding.
func (p *ONNXProvider) extractEmbedding(ctx context.Context, imgBytes []byte) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// --- 1. Decode image ---
	img, err := decodeImage(imgBytes)
	if err != nil {
		return nil, fmt.Errorf("decode image: %w", err)
	}

	// --- 2. Center crop to square (assumes face is centered in the oval UI) ---
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	size := width
	if height < size {
		size = height
	}

	x0 := bounds.Min.X + (width-size)/2
	y0 := bounds.Min.Y + (height-size)/2

	// Create a square rect representing the center
	cropRect := image.Rect(x0, y0, x0+size, y0+size)

	// Draw the cropped portion to a new square image
	cropped := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.Draw(cropped, cropped.Bounds(), img, cropRect.Min, draw.Src)

	// --- 3. Resize square to 112x112 ---
	resized := image.NewRGBA(image.Rect(0, 0, arcfaceInputWidth, arcfaceInputHeight))
	draw.BiLinear.Scale(resized, resized.Bounds(), cropped, cropped.Bounds(), draw.Src, nil)

	// --- 4. Build CHW float32 tensor with BGR format and [-1, 1] normalization ---
	inputData := imageToTensor(resized)

	// --- 4. Run ONNX inference ---
	inputShape := ort.NewShape(1, arcfaceChannels, arcfaceInputHeight, arcfaceInputWidth)
	inputTensor, err := ort.NewTensor(inputShape, inputData)
	if err != nil {
		return nil, fmt.Errorf("create input tensor: %w", err)
	}
	defer func() {
		if err := inputTensor.Destroy(); err != nil {
			log.Printf("failed to destroy input tensor: %v", err)
		}
	}()

	outputShape := ort.NewShape(1, arcfaceEmbeddingSize)
	outputData := make([]float32, arcfaceEmbeddingSize)
	outputTensor, err := ort.NewTensor(outputShape, outputData)
	if err != nil {
		return nil, fmt.Errorf("create output tensor: %w", err)
	}
	defer func() {
		if err := outputTensor.Destroy(); err != nil {
			log.Printf("failed to destroy output tensor: %v", err)
		}
	}()

	session, err := ort.NewAdvancedSession(
		p.modelPath,
		[]string{"input.1"}, // ArcFace ResNet50 input name
		[]string{"683"},     // ArcFace ResNet50 output name
		[]ort.ArbitraryTensor{inputTensor},
		[]ort.ArbitraryTensor{outputTensor},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create ONNX session: %w", err)
	}
	defer func() {
		if err := session.Destroy(); err != nil {
			log.Printf("failed to destroy ONNX session: %v", err)
		}
	}()

	if err := session.Run(); err != nil {
		return nil, fmt.Errorf("ONNX inference failed: %w", err)
	}

	// Make a copy so the tensor can be safely released
	embedding := make([]float32, arcfaceEmbeddingSize)
	copy(embedding, outputTensor.GetData())

	return embedding, nil
}

// decodeImage decodes JPEG or PNG bytes into an image.Image.
func decodeImage(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// Try JPEG explicitly as fallback
		img, err = jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("unsupported image format (expected JPEG or PNG): %w", err)
		}
	}
	return img, nil
}

// imageToTensor converts an RGBA image to a CHW float32 tensor normalized to [-1, 1].
// Channel order is RGB (ArcFace buffalo_l models expect RGB).
func imageToTensor(img *image.RGBA) []float32 {
	size := arcfaceChannels * arcfaceInputHeight * arcfaceInputWidth
	data := make([]float32, size)

	for y := 0; y < arcfaceInputHeight; y++ {
		for x := 0; x < arcfaceInputWidth; x++ {
			pixel := img.RGBAAt(x, y)
			// Normalize [0, 255] → [-1, 1]
			r := (float32(pixel.R)/127.5 - 1.0)
			g := (float32(pixel.G)/127.5 - 1.0)
			b := (float32(pixel.B)/127.5 - 1.0)

			// CHW layout: C=0 → R, C=1 → G, C=2 → B
			baseOffset := y*arcfaceInputWidth + x
			data[0*arcfaceInputHeight*arcfaceInputWidth+baseOffset] = r // Red
			data[1*arcfaceInputHeight*arcfaceInputWidth+baseOffset] = g // Green
			data[2*arcfaceInputHeight*arcfaceInputWidth+baseOffset] = b // Blue
		}
	}
	return data
}

// cosineSimilarity computes the cosine similarity ∈ [-1, 1] between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}
