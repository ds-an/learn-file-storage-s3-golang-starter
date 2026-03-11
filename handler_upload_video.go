package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1 << 30)
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}


	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Video is not present in the db", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized", nil)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType := header.Header.Get("Content-Type")
	if mediaType == "" {
		respondWithError(w, http.StatusBadRequest, "Missing Content-Type for thumbnail", nil)
		return
	}
	mediaTypeSeparate, _, err := mime.ParseMediaType(mediaType)
	if err != nil || mediaTypeSeparate != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Non-video (mp4) content type", nil)
		return
	}

	tempFile, err := os.CreateTemp("", "tubely-upload-*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating temporary video file", err)
		return
	}

	if _, err = io.Copy(tempFile, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error writing video asset file to disk", err)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tempFile.Name())

	videoAspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting video aspect ratio", err)
		return
	}
	switch videoAspectRatio {
	case "16:9":
		videoAspectRatio = "landscape"
	case "9:16":
		videoAspectRatio = "portrait"
	default:
		videoAspectRatio = "other"
	}

	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error setting temporary file's pointer to the beginning", err)
		return
	}

	tempFileProcessedPath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error processing video file for fast start", err)
		return
	}
	tempFileProcessed, err := os.Open(tempFileProcessedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video file", err)
		return
	}

	randBytes := make([]byte, 32)
	rand.Read(randBytes)
	idString := hex.EncodeToString(randBytes)
	fileVideoName := fmt.Sprintf("%s.mp4", idString)
	key := path.Join(videoAspectRatio, fileVideoName)
	putObjectParams := s3.PutObjectInput{
		Bucket: &cfg.s3Bucket,
		Key: &key,
		Body: tempFileProcessed,
		ContentType: &mediaTypeSeparate,
	}

	_, err = cfg.s3Client.PutObject(r.Context(), &putObjectParams)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error putting video in S3 bucket", err)
		return
	}

	videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s/%s", cfg.s3Bucket, cfg.s3Region, videoAspectRatio, fileVideoName)
	video.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}
}

func getVideoAspectRatio(filePath string) (string, error) {
	type aspectRatio struct {
    Streams []struct {
        Width  int `json:"width"`
        Height int `json:"height"`
    } `json:"streams"`
	}
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	videoAspectRatio := aspectRatio{}
	if err := json.Unmarshal(out.Bytes(), &videoAspectRatio); err != nil {
		return "", err
	}
	if len(videoAspectRatio.Streams) == 0 {
		return "", errors.New("no streams found in ffprobe output")
	}

	width := videoAspectRatio.Streams[0].Width
	height := videoAspectRatio.Streams[0].Height
	ratio := float64(width) / float64(height)

	if math.Abs(ratio - 16.0/9.0) < 0.01 {
		return "16:9", nil
	}
	if math.Abs(ratio - 9.0/16.0) < 0.01 {
		return "9:16", nil
	}
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return outputFilePath, nil
}
