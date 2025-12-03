package api

import (
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/features"
)

func RESTMapperProvider(group string) func(*rest.Config, *http.Client) (meta.RESTMapper, error) {
	return func(cfg *rest.Config, cl *http.Client) (meta.RESTMapper, error) {

		standardGV := ocv1.GroupVersion
		customGV := schema.GroupVersion{
			Group:   group,
			Version: "v1",
		}

		remapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{standardGV})
		remapper.AddSpecific(
			standardGV.WithKind("ClusterExtension"),
			customGV.WithResource("clusterextensions"),
			customGV.WithResource("clusterextension"), meta.RESTScopeRoot)
		remapper.AddSpecific(
			standardGV.WithKind("ClusterExtensionList"),
			customGV.WithResource("clusterextensions"),
			customGV.WithResource("clusterextension"), meta.RESTScopeRoot)
		remapper.AddSpecific(
			standardGV.WithKind("ClusterCatalog"),
			standardGV.WithResource("clustercatalogs"),
			standardGV.WithResource("clustercatalog"), meta.RESTScopeRoot)
		remapper.AddSpecific(
			standardGV.WithKind("ClusterCatalogList"),
			standardGV.WithResource("clustercatalogs"),
			standardGV.WithResource("clustercatalog"), meta.RESTScopeRoot)
		if features.OperatorControllerFeatureGate.Enabled(features.BoxcutterRuntime) {
			remapper.AddSpecific(
				standardGV.WithKind("ClusterExtensionRevision"),
				customGV.WithResource("clusterextensionrevisions"),
				customGV.WithResource("clusterextensionrevision"), meta.RESTScopeRoot)
			remapper.AddSpecific(
				standardGV.WithKind("ClusterExtensionRevisionList"),
				customGV.WithResource("clusterextensionrevisions"),
				customGV.WithResource("clusterextensionrevision"), meta.RESTScopeRoot)
		}

		delegate, err := apiutil.NewDynamicRESTMapper(cfg, cl)
		if err != nil {
			return nil, err
		}
		mapper := meta.FirstHitRESTMapper{MultiRESTMapper: meta.MultiRESTMapper{remapper, delegate}}
		return mapper, nil
	}
}
