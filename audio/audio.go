package audio

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

var workingDir string = path.Join(os.TempDir(), "apollo-audio")
var clipDir string = path.Join(workingDir, "clips")
var AAC string = "aac"
var M4A string = "m4a"
var OGG string = "ogg"

/* container for the request variables */
type RequestVars struct {
	Mission  int
	Channels []string
	Format   string
	Start    int
	Duration int
}

/* local paths to audio files for all channels belonging to particular time slice */
type TimeSlice struct {
	start    int
	end      int
	segments map[int]AudioSegment
}

func NewTimeSlice(start, end int) TimeSlice {
	segments := make(map[int]AudioSegment)
	return TimeSlice{start, end, segments}
}

type AudioSegment struct {
	start      int
	end        int
	url        string
	localPath  string
	channel_id int
}

func InitDirs() {
	makeDir(workingDir)
	makeDir(clipDir)
}

func GetRequestSlices(rv RequestVars) []TimeSlice {
	var slices []TimeSlice
	// Get db
	db := connectDb()
	defer db.Close()

	// Psql array for ANY query
	channelString := fmt.Sprintf("{%s}", strings.Join(rv.Channels, ","))
	reqEnd := rv.Start + rv.Duration

	// Query
	stmt, err := db.Prepare("SELECT met_start, met_end, url, channel_id FROM audio_segments WHERE channel_id = ANY($1::integer[]) AND met_end > $2 AND met_start < $3 ORDER BY met_start")
	check(err)
	rows, err := stmt.Query(channelString, rv.Start, reqEnd)
	check(err)
	defer rows.Close()

	//Scan results and build slices
	lastStart := -1
	for rows.Next() {
		var segment AudioSegment
		err = rows.Scan(&segment.start, &segment.end, &segment.url, &segment.channel_id)
		if err != nil {
			log.Println("Error reading row", err)
			continue
		}
		//Check for new timeslice
		if segment.start > lastStart {
			slices = append(slices, NewTimeSlice(segment.start, segment.end))
			lastStart = segment.start
		}
		// Set local name
		loc := fmt.Sprintf("mission_%d_channel_%d_%d.wav", rv.Mission, segment.channel_id, segment.start)
		segment.localPath = path.Join(clipDir, loc)

		// Add to proper slice
		for i, a := range slices {
			if segment.start == a.start && segment.end == a.end {
				slices[i].segments[segment.channel_id] = segment
				break
			}
		}
	}
	return slices
}

func getSoxTrimArgs(i int, rv RequestVars, slices []TimeSlice) (args []string) {
	slice := slices[i]
	trimOffset := 0
	if i == 0 && rv.Start > slice.start {
		trimOffset = rv.Start - slice.start
		offset := float64(trimOffset) / 1000.0
		offStr := strconv.FormatFloat(offset, 'f', 4, 64)
		log.Println("Trimming first slice by", offStr)
		args = append(args, "trim", offStr)
	}
	reqEnd := rv.Start + rv.Duration
	if i == len(slices)-1 && slice.end > reqEnd {
		var duration int
		if rv.Start > slice.start {
			duration = reqEnd - rv.Start
		} else {
			duration = reqEnd - slice.start
		}
		df := float64(duration) / 1000.0
		durStr := strconv.FormatFloat(df, 'f', 4, 64)
		log.Println("Trimming last slice by", durStr)

		// Need starting offset if not already set
		if trimOffset == 0 {
			args = append(args, "trim", "0", durStr)
		} else {
			args = append(args, durStr)
		}
	}
	return args
}

func soxBulkTrimArgs(rv RequestVars, slices []TimeSlice) (args []string) {
	slice := slices[0]
	trimOffset := 0
	//Look at first slice
	if rv.Start > slice.start {
		trimOffset = rv.Start - slice.start
	}
	offset := float64(trimOffset) / 1000.0
	offStr := strconv.FormatFloat(offset, 'f', 4, 64)
	log.Println("Trimming first slice by", offStr)

	df := float64(rv.Duration) / 1000.0
	durStr := strconv.FormatFloat(df, 'f', 4, 64)
	log.Println("Duration is", durStr)

	args = append(args, "trim", offStr, durStr)
	log.Println(args)

	return args
}

func DownloadAndStream(slices []TimeSlice, rv RequestVars, w io.Writer) {
	// Download all here. Could be done concurrently while prev slice is streaming
	DownloadAllAudio(slices)

	// Build cmds
	sox, err := exec.LookPath("sox")
	check(err)
	log.Println("using sox " + sox)
	ffmpeg, err := exec.LookPath("ffmpeg")
	check(err)
	log.Println("using ffmpeg " + ffmpeg)

	// Process and stream each segment
	for i, slice := range slices {
		var segmentPaths []string
		// Gather paths
		for _, ch := range slice.segments {
			segmentPaths = append(segmentPaths, ch.localPath)
		}
		// Merge the channels
		soxArgs := []string{"-t", "wav"}
		// Only merge if there are multiple files
		if len(segmentPaths) > 1 {
			soxArgs = append(soxArgs, "-m")
		}
		soxArgs = append(soxArgs, segmentPaths...)
		soxArgs = append(soxArgs, "-p")

		// Handle trim cases on start and end
		soxArgs = append(soxArgs, getSoxTrimArgs(i, rv, slices)...)

		log.Println("running sox", strings.Join(soxArgs, " "))
		soxCommand := exec.Command(sox, soxArgs...)

		// Transcode the result
		var ffmpegArgs []string
		if rv.Format == AAC || rv.Format == M4A {
			ffmpegArgs = []string{"-i", "-", "-c:a", "libfdk_aac", "-b:a", "256k", "-f", M4A, "pipe:"}
			// works, but gotta compile ffmpeg on server with special options
		} else if rv.Format == OGG {
			ffmpegArgs = []string{"-i", "-", "-c:a", "libvorbis", "-qscale:a", "6", "-f", OGG, "pipe:"}
		} else {
			log.Println("unsupported output format requested. break some rools.")
			ffmpegArgs = []string{"-i", "-", "-f", "mp3", "-ab", "256k", "pipe:"}
		}
		log.Println("running ffmpeg", strings.Join(ffmpegArgs, " "))
		ffmpegCommand := exec.Command(ffmpeg, ffmpegArgs...)

		ffmpegCommand.Stdin, _ = soxCommand.StdoutPipe()
		ffmpegCommand.Stdout = w
		ffmpegCommand.Stderr = os.Stdout

		ffmpegCommand.Start()
		soxCommand.Run()
		ffmpegCommand.Wait()
	}
}

func DownloadAndEncode(slices []TimeSlice, rv RequestVars) string {
	DownloadAllAudio(slices)

	// Build cmds
	soxPath, err := exec.LookPath("sox")
	check(err)
	log.Println("using sox " + soxPath)
	ffmpegPath, err := exec.LookPath("ffmpeg")
	check(err)
	log.Println("using ffmpeg " + ffmpegPath)

	audioName := fmt.Sprintf("channels_%s_%d_%d.m4a", strings.Join(rv.Channels, "."), rv.Start, rv.Duration)
	outFile := path.Join(clipDir, audioName)

	// Merge and concat all files
	// sox "|sox -m in1 in2 -p" "|sox -m in3 in4 -p" -p
	// Use shell for sox to handle funky args
	soxArgs := []string{"sox"}
	for _, slice := range slices {
		var segmentPaths []string
		var cmdStr string
		// Get files for this slice
		for _, ch := range slice.segments {
			segmentPaths = append(segmentPaths, ch.localPath)
		}
		// Only merge with multiple files
		if len(segmentPaths) == 1 {
			cmdStr = segmentPaths[0]
		} else {
			cmdStr = fmt.Sprintf("\"| %s -m %s -p\"", "sox", strings.Join(segmentPaths, " "))
		}
		soxArgs = append(soxArgs, cmdStr)
	}
	soxArgs = append(soxArgs, "-p")

	//Trim file
	soxArgs = append(soxArgs, soxBulkTrimArgs(rv, slices)...)

	ffmpegArgs := []string{"-i", "-", "-strict", "-2", "-c:a", "aac", "-b:a", "96k", "-f", "mp4", outFile}
	ffmpegArgs = append(ffmpegArgs, "-y") // Force ovewrite

	soxCmd := strings.Join(soxArgs, " ")
	log.Println("running sox", soxCmd)
	soxCommand := exec.Command("sh", "-c", soxCmd)

	log.Println("running ffmpeg", strings.Join(ffmpegArgs, " "))
	ffmpegCommand := exec.Command(ffmpegPath, ffmpegArgs...)

	ffmpegCommand.Stdin, err = soxCommand.StdoutPipe()
	if err != nil {
		log.Fatal("Cant connect sox pipe")
	}
	ffmpegCommand.Stdout = os.Stdout
	ffmpegCommand.Stderr = os.Stdout
	soxCommand.Stderr = os.Stdout

	ffmpegCommand.Start()
	soxCommand.Run()
	ffmpegCommand.Wait()

	log.Println("Saved output to", outFile)
	return outFile
}
