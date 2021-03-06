package tags

import (
	"fmt"
	"regexp"

	"os"

	manifestV1 "github.com/docker/distribution/manifest/schema1"

	"github.com/Jeffail/tunny"
	"github.com/docker/distribution/manifest"
	"github.com/docker/libtrust"
	"github.com/Dreamlol/promoter/connection"
	"github.com/Dreamlol/promoter/progressbar"
	"gopkg.in/cheggaaa/pb.v1"
	"io/ioutil"
	"log"
)

//TagPush holds image tags promotion structure
type TagPush struct {
	SrcRegistry  string
	SrcImage     string
	SrcUsername  string
	SrcPassword  string
	SrcInsecure  bool
	DestRegistry string
	DestImage    string
	DestUsername string
	DestPassword string
	DestInsecure bool
	TagRegexp    string
	Debug        bool
}
type manifestGetResult struct {
	manifest manifestV1.SignedManifest
	tag      string
	err      error
}
type layerCheck struct {
	layer       manifestV1.FSLayer
	size        int64
	remoteExist bool
	err         error
}
type uploadResult struct {
	layer manifestV1.FSLayer
	err   error
}
type manifestDeployResult struct {
	destManifest manifestV1.SignedManifest
	err          error
}

//PushTags promotes all specified image tags.
func (th *TagPush) PushTags() {
	if !th.Debug {
		log.SetOutput(ioutil.Discard)
	}
	fmt.Println("Preparing tags push")
	srcHub, destHub := connection.InitConnection(th.SrcRegistry, th.SrcUsername, th.SrcPassword, th.SrcInsecure, th.DestRegistry, th.DestUsername, th.DestPassword, th.DestInsecure)
	fmt.Println("Source Image: " + th.SrcImage)
	fmt.Println("Destination image: " + th.DestImage)
	tags, err := srcHub.Tags(th.SrcImage)
	if err != nil {
		fmt.Println("Error occurred while trying to get Source Image Tags")
		fmt.Println("Error: " + err.Error())
		os.Exit(1)
	}

	totalTags := len(tags)

	fmt.Printf("Source image contains %d tags\n", totalTags)

	if len(th.TagRegexp) > 0 {
		tags, err = filterByVersionSelector(tags, th.TagRegexp)
		if err != nil {
			fmt.Printf("Failed to filter by provided Tag regexp. Error: %q \n", err)
			os.Exit(1)
		} else {
			fmt.Printf("Tag regexp matched %d images \n", len(tags))
		}
		if len(tags) == 0 {
			fmt.Println("Image Tag Regexp didn't matched any tags")
			os.Exit(1)
		}
	}

	layers := make([]manifestV1.FSLayer, 0)
	manifests := make([]manifestGetResult, 0)
	//TO-DO parametrize number of connections
	poolSize := 5

	manifestGetQueue := tunny.NewFunc(poolSize, func(payload interface{}) interface{} {
		tag := payload.(string)
		manifest, err := srcHub.Manifest(th.SrcImage, tag)
		if err != nil {
			return &manifestGetResult{
				err: err,
				tag: tag,
			}
		}
		return &manifestGetResult{
			manifest: *manifest,
			tag:      tag,
			err:      nil,
		}
	})
	defer manifestGetQueue.Close()

	manifestGetResultChannel := make(chan *manifestGetResult)
	//Submit manifest retrieval
	for i := 0; i < len(tags); i++ {
		go func(tag string) {
			result := manifestGetQueue.Process(tag)
			manifestGetResultChannel <- result.(*manifestGetResult)
		}(tags[i])
	}
	manifestGetProgressBar := pb.New(len(tags)).SetUnits(pb.U_NO)
	manifestGetProgressBar.Start()
	//Pull manifest
	for i := 0; i < len(tags); i++ {
		res := <-manifestGetResultChannel
		manifestGetProgressBar.Add(1)
		manifests = append(manifests, *res)
	}
	manifestGetProgressBar.Finish()

	for i := 0; i < len(manifests); i++ {
		layers = append(layers, manifests[i].manifest.FSLayers...)
	}
	fmt.Printf("Total number of layers %d \n", len(layers))
	uniqueLayers := make([]manifestV1.FSLayer, 0)

	for _, layer := range layers {
		uniqueLayers = appendIfMissing(uniqueLayers, layer)
	}
	if len(layers) > len(uniqueLayers) {
		duplicateLayerCount := len(layers) - len(uniqueLayers)
		fmt.Printf("Reducing transfer size by skipping duplicate layers. Duplicate layers skipped: %d \n", duplicateLayerCount)
	}
	fmt.Println("Retrieving layer metadata and optimising transfer..")

	layerSizeGetQueue := tunny.NewFunc(10, func(payload interface{}) interface{} {
		layer := payload.(manifestV1.FSLayer)
		metadata, err := srcHub.LayerMetadata(th.SrcImage, layer.BlobSum)
		if err != nil {
			return &layerCheck{
				layer: layer,
				err:   err,
			}
		}
		return &layerCheck{
			layer: layer,
			size:  metadata.Size,
			err:   nil,
		}
	})
	layerExistQueue := tunny.NewFunc(5, func(payload interface{}) interface{} {
		layerCheck := payload.(*layerCheck)
		//If previous operation failed then pass layer exist check
		if layerCheck.err != nil {
			return layerCheck
		}
		exist, _ := destHub.HasLayer(th.DestImage, layerCheck.layer.BlobSum)
		layerCheck.remoteExist = exist
		return layerCheck
	})
	defer layerSizeGetQueue.Close()
	defer layerExistQueue.Close()

	layerCheckProgressBar := pb.New(len(uniqueLayers)).SetUnits(pb.U_NO)
	layerCheckProgressBar.Start()

	layerCheckChannel := make(chan *layerCheck)
	for i := 0; i < len(uniqueLayers); i++ {
		go func(layer manifestV1.FSLayer) {
			result := layerSizeGetQueue.Process(layer)
			result = layerExistQueue.Process(result.(*layerCheck))
			layerCheckChannel <- result.(*layerCheck)
		}(uniqueLayers[i])
	}

	layerCheckResults := make([]layerCheck, 0)
	for i := 0; i < len(uniqueLayers); i++ {
		res := <-layerCheckChannel
		layerCheckProgressBar.Add(1)
		layerCheckResults = append(layerCheckResults, *res)
	}
	layerCheckProgressBar.Finish()

	fmt.Println("Transferring layers...")
	var totalReader = make(chan int64)
	uploadResultChannel := make(chan *uploadResult)
	uploadResults := make([]uploadResult, 0)
	uploadQueue := tunny.NewFunc(poolSize, func(payload interface{}) interface{} {
		upload := payload.(manifestV1.FSLayer)
		reader, err := srcHub.DownloadLayer(th.SrcImage, upload.BlobSum)
		if reader != nil {
			defer reader.Close()
		}
		var err2 error
		if totalReader != nil {
			rd := &progressbar.PassThru{ReadCloser: reader, Total: &totalReader}
			err2 = destHub.UploadLayer(th.DestImage, upload.BlobSum, rd)
		} else {
			err2 = destHub.UploadLayer(th.DestImage, upload.BlobSum, reader)
		}
		if err != nil {
			fmt.Printf("Error occurred while uploading layer:  %s. Error: %s \n", upload.BlobSum, err.Error())
		}
		if err2 != nil {
			fmt.Printf("Error occurred while uploading layer:  %s. Error: %s \n", upload.BlobSum, err2.Error())
		}

		return &uploadResult{
			layer: upload,
			err:   err,
		}
	})
	defer uploadQueue.Close()

	//Get total transfer size
	var transferSize int64
	for _, layerCheckResult := range layerCheckResults {
		if !layerCheckResult.remoteExist {
			transferSize = transferSize + layerCheckResult.size
		}
	}
	uploadProgressBar := pb.New64(transferSize * 2).SetUnits(pb.U_BYTES)
	uploadProgressBar.Start()

	//Submit upload
	for _, layerCheckResult := range layerCheckResults {
		if layerCheckResult.err == nil && !layerCheckResult.remoteExist {
			go func(layer manifestV1.FSLayer) {
				result := uploadQueue.Process(layer)
				uploadResultChannel <- result.(*uploadResult)
			}(layerCheckResult.layer)
		}
		if layerCheckResult.err != nil {
			fmt.Printf("Failed to retrieve layer %s data. Error: %s \n", layerCheckResult.layer.BlobSum, layerCheckResult.err.Error())
		}
	}
	//Constantly update progress bar
	go func() {
		for {
			t := <-totalReader
			uploadProgressBar.Add64(t * 2)
		}
	}()

	//Collect upload results
	for _, layerCheckResult := range layerCheckResults {
		if layerCheckResult.err == nil && !layerCheckResult.remoteExist {
			res := <-uploadResultChannel
			uploadResults = append(uploadResults, *res)
		}
	}
	uploadProgressBar.Finish()

	//Deploy manifest files
	fmt.Println("Uploading Manifest files...")
	key, err := libtrust.GenerateECP256PrivateKey()
	manifestDeployResultChannel := make(chan *manifestDeployResult)
	manifestDeployResults := make([]manifestDeployResult, 0)
	manifestDeployQueue := tunny.NewFunc(poolSize, func(payload interface{}) interface{} {
		srcManifest := payload.(manifestV1.SignedManifest)
		destManifest := &manifestV1.Manifest{
			Versioned: manifest.Versioned{
				SchemaVersion: 1,
			},
			Name:         th.DestImage,
			Tag:          srcManifest.Tag,
			Architecture: srcManifest.Architecture,
			FSLayers:     srcManifest.FSLayers,
			History:      srcManifest.History,
		}
		signedDestManifest, err := manifestV1.Sign(destManifest, key)
		if err != nil {
			return &manifestDeployResult{
				destManifest: *signedDestManifest,
				err:          err,
			}
		}
		err = destHub.PutManifest(th.DestImage, srcManifest.Tag, signedDestManifest)

		return &manifestDeployResult{
			destManifest: *signedDestManifest,
			err:          err,
		}
	})
	defer manifestDeployQueue.Close()

	for i := 0; i < len(manifests); i++ {
		if manifests[i].err == nil {
			go func(manifest manifestV1.SignedManifest) {
				result := manifestDeployQueue.Process(manifest)
				manifestDeployResultChannel <- result.(*manifestDeployResult)
			}(manifests[i].manifest)

		}
	}
	manifestDeployProgressBar := pb.New(len(manifests)).SetUnits(pb.U_NO)
	manifestDeployProgressBar.Start()

	//Collect manifest deployment results
	for i := 0; i < len(manifests); i++ {
		manifestDeployProgressBar.Add(1)
		if manifests[i].err == nil {
			res := <-manifestDeployResultChannel
			manifestDeployResults = append(manifestDeployResults, *res)
		}
	}
	manifestDeployProgressBar.Finish()
	//Report failed deployments
	var errorsFound bool
	for i := 0; i < len(manifests); i++ {
		if manifests[i].err != nil {
			fmt.Printf("Failed to push image %s because unable to retrieve image manifest. Error: %s \n", manifests[i].manifest.Name+":"+manifests[i].tag, manifests[i].err.Error())
			errorsFound = true
		}
	}
	for _, manifestDeployResult := range manifestDeployResults {
		if manifestDeployResult.err != nil {
			fmt.Printf("Failed to push image %s because unable to deploy image manifest. Error: %s \n", manifestDeployResult.destManifest.Name+":"+manifestDeployResult.destManifest.Tag, manifestDeployResult.err.Error())
			errorsFound = true
		}
	}
	fmt.Println("All done!")
	if errorsFound {
		os.Exit(1)
	}
	os.Exit(0)
}
func appendIfMissing(slice []manifestV1.FSLayer, i manifestV1.FSLayer) []manifestV1.FSLayer {
	for _, ele := range slice {
		if ele == i {
			return slice
		}
	}
	return append(slice, i)
}
func filterByVersionSelector(tags []string, filter string) (tagsFiltered []string, err error) {

	filteredTags := make([]string, 0)
	r, err := regexp.Compile(filter)
	if err == nil {
		for _, tag := range tags {
			match := r.MatchString(tag)
			if match {
				filteredTags = append(filteredTags, tag)
			}
		}
	}
	return filteredTags, err
}
