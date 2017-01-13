package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/docker/distribution"
	"github.com/docker/distribution/configuration"
	"github.com/docker/distribution/context"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/manifest/schema1"
	"github.com/docker/distribution/registry/handlers"
	_ "github.com/docker/distribution/registry/storage/driver/inmemory"

	"k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/fake"

	"github.com/openshift/origin/pkg/client/testclient"
	registrytest "github.com/openshift/origin/pkg/dockerregistry/testutil"
	imagetest "github.com/openshift/origin/pkg/image/admission/testutil"
	imageapi "github.com/openshift/origin/pkg/image/api"
)

func TestPullthroughManifests(t *testing.T) {
	ctx := context.Background()

	installFakeAccessController(t)

	testImage, err := registrytest.NewImageForManifest("user/app", registrytest.SampleImageManifestSchema1, false)
	if err != nil {
		t.Fatal(err)
	}
	testImage.DockerImageManifest = ""

	client := &testclient.Fake{}
	client.AddReactor("get", "images", registrytest.GetFakeImageGetHandler(t, *testImage))

	// TODO: get rid of those nasty global vars
	backupRegistryClient := DefaultRegistryClient
	DefaultRegistryClient = makeFakeRegistryClient(client, fake.NewSimpleClientset())
	defer func() {
		// set it back once this test finishes to make other unit tests working again
		DefaultRegistryClient = backupRegistryClient
	}()

	// pullthrough middleware will attempt to pull from this registry instance
	remoteRegistryApp := handlers.NewApp(ctx, &configuration.Configuration{
		Loglevel: "debug",
		Auth: map[string]configuration.Parameters{
			fakeAuthorizerName: {"realm": fakeAuthorizerName},
		},
		Storage: configuration.Storage{
			"inmemory": configuration.Parameters{},
			"cache": configuration.Parameters{
				"blobdescriptor": "inmemory",
			},
			"delete": configuration.Parameters{
				"enabled": true,
			},
		},
		Middleware: map[string][]configuration.Middleware{
			"registry":   {{Name: "openshift"}},
			"repository": {{Name: "openshift"}},
			"storage":    {{Name: "openshift"}},
		},
	})
	remoteRegistryServer := httptest.NewServer(remoteRegistryApp)
	defer remoteRegistryServer.Close()

	serverURL, err := url.Parse(remoteRegistryServer.URL)
	if err != nil {
		t.Fatalf("error parsing server url: %v", err)
	}
	os.Setenv("DOCKER_REGISTRY_URL", serverURL.Host)
	testImage.DockerImageReference = fmt.Sprintf("%s/%s@%s", serverURL.Host, "user/app", testImage.Name)

	testImageStream := registrytest.TestNewImageStreamObject("user", "app", "latest", testImage.Name, testImage.DockerImageReference)
	if testImageStream.Annotations == nil {
		testImageStream.Annotations = make(map[string]string)
	}
	testImageStream.Annotations[imageapi.InsecureRepositoryAnnotation] = "true"

	client.AddReactor("get", "imagestreams", imagetest.GetFakeImageStreamGetHandler(t, *testImageStream))

	signedManifest := &schema1.SignedManifest{}
	if err := json.Unmarshal([]byte(etcdManifest), signedManifest); err != nil {
		t.Fatalf("error unmarshaling signed manifest: %v", err)
	}

	for _, tc := range []struct {
		name                  string
		method                string
		manifestDigest        digest.Digest
		localData             map[digest.Digest]distribution.Manifest
		expectedLocalCalls    map[string]int
		expectedError         bool
		expectedNotFoundError bool
	}{
		{
			name:           "manifest digest",
			method:         "GET",
			manifestDigest: etcdDigest,
			localData: map[digest.Digest]distribution.Manifest{
				etcdDigest: signedManifest,
			},
			expectedLocalCalls: map[string]int{
				"Get": 1,
			},
		},
		{
			name:                  "unknown manifest digest",
			method:                "GET",
			manifestDigest:        unknownBlobDigest,
			expectedNotFoundError: true,
			expectedLocalCalls: map[string]int{
				"Get": 1,
			},
		},
	} {
		localManifestService := newTestManifestService(tc.localData)

		cachedLayers, err := newDigestToRepositoryCache(10)
		if err != nil {
			t.Fatal(err)
		}

		ptms := &pullthroughManifestService{
			ManifestService: localManifestService,
			repo: &repository{
				ctx:              ctx,
				namespace:        "user",
				name:             "app",
				pullthrough:      true,
				cachedLayers:     cachedLayers,
				registryOSClient: client,
			},
		}

		manifestResult, err := ptms.Get(ctx, tc.manifestDigest)
		switch err.(type) {
		case distribution.ErrManifestUnknownRevision:
			if !tc.expectedNotFoundError {
				t.Fatalf("[%s] unexpected error: %#+v", tc.name, err)
			}
		case nil:
			if tc.expectedError || tc.expectedNotFoundError {
				t.Fatalf("[%s] unexpected successful response", tc.name)
			}
		default:
			if tc.expectedError {
				break
			}
			if tc.expectedNotFoundError && err == distribution.ErrBlobUnknown {
				break
			}
			t.Fatalf("[%s] unexpected error: %#+v", tc.name, err)
		}

		if manifestResult != nil && manifestResult != tc.localData[tc.manifestDigest] {
			t.Fatalf("[%s] unexpected retult returned", tc.name)
		}

		for name, count := range localManifestService.calls {
			expectCount, exists := tc.expectedLocalCalls[name]
			if !exists {
				t.Errorf("[%s] expected no calls to method %s of local manifest service, got %d", tc.name, name, count)
			}
			if count != expectCount {
				t.Errorf("[%s] unexpected number of calls to method %s of local manifest service, got %d", tc.name, name, count)
			}
		}
	}
}

type testManifestService struct {
	data  map[digest.Digest]distribution.Manifest
	calls map[string]int
}

var _ distribution.ManifestService = &testManifestService{}

func newTestManifestService(data map[digest.Digest]distribution.Manifest) *testManifestService {
	b := make(map[digest.Digest]distribution.Manifest)
	for d, content := range data {
		b[d] = content
	}
	return &testManifestService{
		data:  b,
		calls: make(map[string]int),
	}
}

func (t *testManifestService) Exists(ctx context.Context, dgst digest.Digest) (bool, error) {
	t.calls["Exists"]++
	_, exists := t.data[dgst]
	return exists, nil
}

func (t *testManifestService) Get(ctx context.Context, dgst digest.Digest, options ...distribution.ManifestServiceOption) (distribution.Manifest, error) {
	t.calls["Get"]++
	content, exists := t.data[dgst]
	if !exists {
		return nil, distribution.ErrBlobUnknown
	}
	return content, nil
}

func (t *testManifestService) Put(ctx context.Context, manifest distribution.Manifest, options ...distribution.ManifestServiceOption) (digest.Digest, error) {
	t.calls["Put"]++
	return "", fmt.Errorf("method not implemented")
}

func (t *testManifestService) Delete(ctx context.Context, dgst digest.Digest) error {
	t.calls["Delete"]++
	return fmt.Errorf("method not implemented")
}

const etcdDigest = "sha256:958608f8ecc1dc62c93b6c610f3a834dae4220c9642e6e8b4e0f2b3ad7cbd238"
const etcdManifest = `{
   "schemaVersion": 1, 
   "tag": "latest", 
   "name": "coreos/etcd", 
   "architecture": "amd64", 
   "fsLayers": [
      {
         "blobSum": "sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4"
      }, 
      {
         "blobSum": "sha256:a3ed95caeb02ffe68cdd9fd84406680ae93d633cb16422d00e8a7c22955b46d4"
      }, 
      {
         "blobSum": "sha256:2560187847cadddef806eaf244b7755af247a9dbabb90ca953dd2703cf423766"
      }, 
      {
         "blobSum": "sha256:744b46d0ac8636c45870a03830d8d82c20b75fbfb9bc937d5e61005d23ad4cfe"
      }
   ], 
   "history": [
      {
         "v1Compatibility": "{\"id\":\"fe50ac14986497fa6b5d2cc24feb4a561d01767bc64413752c0988cb70b0b8b9\",\"parent\":\"a5a18474fa96a3c6e240bc88e41de2afd236520caf904356ad9d5f8d875c3481\",\"created\":\"2015-12-30T22:29:13.967754365Z\",\"container\":\"c8d0f1a274b5f52fa5beb280775ef07cf18ec0f95e5ae42fbad01157e2614d42\",\"container_config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"ExposedPorts\":{\"2379/tcp\":{},\"2380/tcp\":{},\"4001/tcp\":{},\"7001/tcp\":{}},\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":[\"/bin/sh\",\"-c\",\"#(nop) ENTRYPOINT \\u0026{[\\\"/etcd\\\"]}\"],\"Image\":\"a5a18474fa96a3c6e240bc88e41de2afd236520caf904356ad9d5f8d875c3481\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":[\"/etcd\"],\"OnBuild\":null,\"Labels\":{}},\"docker_version\":\"1.9.1\",\"config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"ExposedPorts\":{\"2379/tcp\":{},\"2380/tcp\":{},\"4001/tcp\":{},\"7001/tcp\":{}},\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":null,\"Image\":\"a5a18474fa96a3c6e240bc88e41de2afd236520caf904356ad9d5f8d875c3481\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":[\"/etcd\"],\"OnBuild\":null,\"Labels\":{}},\"architecture\":\"amd64\",\"os\":\"linux\"}"
      }, 
      {
         "v1Compatibility": "{\"id\":\"a5a18474fa96a3c6e240bc88e41de2afd236520caf904356ad9d5f8d875c3481\",\"parent\":\"796d581500e960cc02095dcdeccf55db215b8e54c57e3a0b11392145ffe60cf6\",\"created\":\"2015-12-30T22:29:13.504159783Z\",\"container\":\"080708d544f85052a46fab72e701b4358c1b96cb4b805a5b2d66276fc2aaf85d\",\"container_config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"ExposedPorts\":{\"2379/tcp\":{},\"2380/tcp\":{},\"4001/tcp\":{},\"7001/tcp\":{}},\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":[\"/bin/sh\",\"-c\",\"#(nop) EXPOSE 2379/tcp 2380/tcp 4001/tcp 7001/tcp\"],\"Image\":\"796d581500e960cc02095dcdeccf55db215b8e54c57e3a0b11392145ffe60cf6\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":{}},\"docker_version\":\"1.9.1\",\"config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"ExposedPorts\":{\"2379/tcp\":{},\"2380/tcp\":{},\"4001/tcp\":{},\"7001/tcp\":{}},\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":null,\"Image\":\"796d581500e960cc02095dcdeccf55db215b8e54c57e3a0b11392145ffe60cf6\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":{}},\"architecture\":\"amd64\",\"os\":\"linux\"}"
      }, 
      {
         "v1Compatibility": "{\"id\":\"796d581500e960cc02095dcdeccf55db215b8e54c57e3a0b11392145ffe60cf6\",\"parent\":\"309c960c7f875411ae2ee2bfb97b86eee5058f3dad77206dd0df4f97df8a77fa\",\"created\":\"2015-12-30T22:29:12.912813629Z\",\"container\":\"f28be899c9b8680d4cf8585e663ad20b35019db062526844e7cfef117ce9037f\",\"container_config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":[\"/bin/sh\",\"-c\",\"#(nop) ADD file:e330b1da49d993059975e46560b3bd360691498b0f2f6e00f39fc160cf8d4ec3 in /\"],\"Image\":\"309c960c7f875411ae2ee2bfb97b86eee5058f3dad77206dd0df4f97df8a77fa\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":{}},\"docker_version\":\"1.9.1\",\"config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":null,\"Image\":\"309c960c7f875411ae2ee2bfb97b86eee5058f3dad77206dd0df4f97df8a77fa\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":{}},\"architecture\":\"amd64\",\"os\":\"linux\",\"Size\":13502144}"
      }, 
      {
         "v1Compatibility": "{\"id\":\"309c960c7f875411ae2ee2bfb97b86eee5058f3dad77206dd0df4f97df8a77fa\",\"created\":\"2015-12-30T22:29:12.346834862Z\",\"container\":\"1b97abade59e4b5b935aede236980a54fb500cd9ee5bd4323c832c6d7b3ffc6e\",\"container_config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":[\"/bin/sh\",\"-c\",\"#(nop) ADD file:74912593c6783292c4520514f5cc9313acbd1da0f46edee0fdbed2a24a264d6f in /\"],\"Image\":\"\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":null},\"docker_version\":\"1.9.1\",\"config\":{\"Hostname\":\"1b97abade59e\",\"Domainname\":\"\",\"User\":\"\",\"AttachStdin\":false,\"AttachStdout\":false,\"AttachStderr\":false,\"Tty\":false,\"OpenStdin\":false,\"StdinOnce\":false,\"Env\":null,\"Cmd\":null,\"Image\":\"\",\"Volumes\":null,\"WorkingDir\":\"\",\"Entrypoint\":null,\"OnBuild\":null,\"Labels\":null},\"architecture\":\"amd64\",\"os\":\"linux\",\"Size\":15141568}"
      }
   ], 
   "signatures": [
      {
         "header": {
            "alg": "RS256", 
            "jwk": {
               "e": "AQAB", 
               "kty": "RSA", 
               "n": "yB40ou1GMvIxYs1jhxWaeoDiw3oa0_Q2UJThUPtArvO0tRzaun9FnSphhOEHIGcezfq95jy-3MN-FIjmsWgbPHY8lVDS38fF75aCw6qkholwqjmMtUIgPNYoMrg0rLUE5RRyJ84-hKf9Fk7V3fItp1mvCTGKaS3ze-y5dTTrfbNGE7qG638Dla2Fuz-9CNgRQj0JH54o547WkKJC-pG-j0jTDr8lzsXhrZC7lJas4yc-vpt3D60iG4cW_mkdtIj52ZFEgHZ56sUj7AhnNVly0ZP9W1hmw4xEHDn9WLjlt7ivwARVeb2qzsNdguUitcI5hUQNwpOVZ_O3f1rUIL_kRw"
            }
         }, 
         "protected": "eyJmb3JtYXRUYWlsIjogIkNuMCIsICJmb3JtYXRMZW5ndGgiOiA1OTI2LCAidGltZSI6ICIyMDE2LTAxLTAyVDAyOjAxOjMzWiJ9", 
         "signature": "DrQ43UWeit-thDoRGTCP0Gd2wL5K2ecyPhHo_au0FoXwuKODja0tfwHexB9ypvFWngk-ijXuwO02x3aRIZqkWpvKLxxzxwkrZnPSje4o_VrFU4z5zwmN8sJw52ODkQlW38PURIVksOxCrb0zRl87yTAAsUAJ_4UUPNltZSLnhwy-qPb2NQ8ghgsONcBxRQrhPFiWNkxDKZ3kjvzYyrXDxTcvwK3Kk_YagZ4rCOhH1B7mAdVSiSHIvvNV5grPshw_ipAoqL2iNMsxWxLjYZl9xSJQI2asaq3fvh8G8cZ7T-OahDUos_GyhnIj39C-9ouqdJqMUYFETqbzRCR6d36CpQ"
      }
   ]
}`