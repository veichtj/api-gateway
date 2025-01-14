package validation

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/kyma-incubator/api-gateway/internal/helpers"
	networkingv1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"

	gatewayv1alpha1 "github.com/kyma-incubator/api-gateway/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
)

//Validators for AccessStrategies
var vldNoConfig = &noConfigAccStrValidator{}
var vldJWT = &jwtAccStrValidator{}
var vldDummy = &dummyAccStrValidator{}

type accessStrategyValidator interface {
	Validate(attrPath string, Handler *gatewayv1alpha1.Handler) []Failure
}

//configNotEmpty Verify if the config object is not empty
func configEmpty(config *runtime.RawExtension) bool {

	return config == nil ||
		len(config.Raw) == 0 ||
		bytes.Equal(config.Raw, []byte("null")) ||
		bytes.Equal(config.Raw, []byte("{}"))
}

//configNotEmpty Verify if the config object is not empty
func configNotEmpty(config *runtime.RawExtension) bool {
	return !configEmpty(config)
}

//APIRule is used to validate github.com/kyma-incubator/api-gateway/api/v1alpha1/APIRule instances
type APIRule struct {
	ServiceBlockList  map[string][]string
	DomainAllowList   []string
	HostBlockList     []string
	DefaultDomainName string
}

//Validate performs APIRule validation
func (v *APIRule) Validate(api *gatewayv1alpha1.APIRule, vsList networkingv1beta1.VirtualServiceList) []Failure {

	res := []Failure{}
	//Validate service
	res = append(res, v.validateService(".spec.service", vsList, api)...)
	//Validate Gateway
	res = append(res, v.validateGateway(".spec.gateway", api.Spec.Gateway)...)
	//Validate Rules
	res = append(res, v.validateRules(".spec.rules", api.Spec.Rules)...)

	return res
}

//Failure carries validation failures for a single attribute of an object.
type Failure struct {
	AttributePath string
	Message       string
}

func (v *APIRule) validateService(attributePath string, vsList networkingv1beta1.VirtualServiceList, api *gatewayv1alpha1.APIRule) []Failure {
	var problems []Failure

	host := *api.Spec.Service.Host
	if !helpers.HostIncludesDomain(*api.Spec.Service.Host) {
		if v.DefaultDomainName == "" {
			problems = append(problems, Failure{
				AttributePath: attributePath + ".host",
				Message:       "Host does not contain a domain name and no default domain name is configured",
			})
		}
		host = helpers.GetHostWithDefaultDomain(host, v.DefaultDomainName)
	} else if len(v.DomainAllowList) > 0 {
		// Do the allowList check only if the list is actually provided AND the default domain name is not used.
		domainFound := false
		for _, domain := range v.DomainAllowList {
			// service host containing duplicated allowlisted domain should be rejected.
			// for example `my-lambda.kyma.local.kyma.local`
			// service host containing allowlisted domain but only as a part of bigger domain should also be rejected
			// for example `my-lambda.kyma.local.com` when only `kyma.local` is allowlisted
			if count := strings.Count(host, domain); count == 1 && strings.HasSuffix(host, domain) {
				domainFound = true
			}
		}
		if !domainFound {
			problems = append(problems, Failure{
				AttributePath: attributePath + ".host",
				Message:       "Host is not allowlisted",
			})
		}
	}

	for _, blockedHost := range v.HostBlockList {
		host := *api.Spec.Service.Host
		if blockedHost == host {
			subdomain := strings.Split(host, ".")[0]
			problems = append(problems, Failure{
				AttributePath: attributePath + ".host",
				Message:       fmt.Sprintf("The subdomain %s is blocklisted for %s domain", subdomain, v.DefaultDomainName),
			})
		}
	}

	for _, vs := range vsList.Items {
		if occupiesHost(vs, host) && !ownedBy(vs, api) {
			problems = append(problems, Failure{
				AttributePath: attributePath + ".host",
				Message:       "This host is occupied by another Virtual Service",
			})
		}
	}

	for namespace, services := range v.ServiceBlockList {
		for _, svc := range services {
			if svc == *api.Spec.Service.Name && namespace == api.ObjectMeta.Namespace {
				problems = append(problems, Failure{
					AttributePath: attributePath + ".name",
					Message:       fmt.Sprintf("Service %s in namespace %s is blocklisted", svc, namespace),
				})
			}
		}
	}
	return problems
}

func (v *APIRule) validateGateway(attributePath string, gateway *string) []Failure {
	return nil
}

func (v *APIRule) validateRules(attributePath string, rules []gatewayv1alpha1.Rule) []Failure {
	var problems []Failure

	if len(rules) == 0 {
		problems = append(problems, Failure{AttributePath: attributePath, Message: "No rules defined"})
		return problems
	}

	if hasDuplicates(rules) {
		problems = append(problems, Failure{AttributePath: attributePath, Message: "multiple rules defined for the same path"})
	}

	for i, r := range rules {
		attrPath := fmt.Sprintf("%s[%d]", attributePath, i)
		problems = append(problems, v.validateMethods(attrPath+".methods", r.Methods)...)
		problems = append(problems, v.validateAccessStrategies(attrPath+".accessStrategies", r.AccessStrategies)...)
	}

	return problems
}

func (v *APIRule) validateMethods(attributePath string, methods []string) []Failure {
	return nil
}

func (v *APIRule) validateAccessStrategies(attributePath string, accessStrategies []*gatewayv1alpha1.Authenticator) []Failure {
	var problems []Failure

	if len(accessStrategies) == 0 {
		problems = append(problems, Failure{AttributePath: attributePath, Message: "No accessStrategies defined"})
		return problems
	}

	for i, r := range accessStrategies {
		strategyAttrPath := attributePath + fmt.Sprintf("[%d]", i)
		problems = append(problems, v.validateAccessStrategy(strategyAttrPath, r)...)
	}

	return problems
}

func (v *APIRule) validateAccessStrategy(attributePath string, accessStrategy *gatewayv1alpha1.Authenticator) []Failure {
	var problems []Failure

	var vld accessStrategyValidator

	switch accessStrategy.Handler.Name {
	case "allow": //our internal constant, does not exist in ORY
		vld = vldNoConfig
	case "noop":
		vld = vldNoConfig
	case "unauthorized":
		vld = vldNoConfig
	case "anonymous":
		vld = vldNoConfig
	case "cookie_session":
		vld = vldNoConfig
	case "oauth2_client_credentials":
		vld = vldDummy
	case "oauth2_introspection":
		vld = vldDummy
	case "jwt":
		vld = vldJWT
	default:
		problems = append(problems, Failure{AttributePath: attributePath + ".handler", Message: fmt.Sprintf("Unsupported accessStrategy: %s", accessStrategy.Handler.Name)})
		return problems
	}

	return vld.Validate(attributePath, accessStrategy.Handler)
}

func occupiesHost(vs networkingv1beta1.VirtualService, host string) bool {
	for _, h := range vs.Spec.Hosts {
		if h == host {
			return true
		}
	}
	return false
}

func ownedBy(vs networkingv1beta1.VirtualService, api *gatewayv1alpha1.APIRule) bool {
	for _, or := range vs.OwnerReferences {
		if or.UID == api.UID {
			return true
		}
	}
	return false
}
