package rbac

import (
	"context"
	"fmt"
	"time"

	"github.com/casbin/casbin"
	"github.com/casbin/casbin/model"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gobuffalo/packr"
	scas "github.com/qiangmzsx/string-adapter"
	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	ConfigMapPolicyCSVKey     = "policy.csv"
	ConfigMapPolicyDefaultKey = "policy.default"

	builtinModelFile      = "model.conf"
	defaultRBACSyncPeriod = 10 * time.Minute
)

// Enforcer is a wrapper around an Casbin enforcer that:
// * is backed by a kubernetes config map
// * has a predefined RBAC model
// * supports a built-in policy
// * supports a user-defined bolicy
// * supports a custom JWT claims enforce function
type Enforcer struct {
	*casbin.Enforcer
	adapter            *scas.Adapter
	clientset          kubernetes.Interface
	namespace          string
	configmap          string
	claimsEnforcerFunc ClaimsEnforcerFunc

	model             model.Model
	defaultRole       string
	builtinPolicy     string
	userDefinedPolicy string
}

// ClaimsEnforcerFunc is func template to enforce a JWT claims. The subject is replaced
type ClaimsEnforcerFunc func(claims jwt.Claims, rvals ...interface{}) bool

var (
	modelConf string
)

func init() {
	box := packr.NewBox(".")
	modelConf = box.String(builtinModelFile)
}

func NewEnforcer(clientset kubernetes.Interface, namespace, configmap string, claimsEnforcer ClaimsEnforcerFunc) *Enforcer {
	adapter := scas.NewAdapter("")
	builtInModel := newBuiltInModel()
	enf := casbin.NewEnforcer(builtInModel, adapter)
	enf.EnableLog(false)
	return &Enforcer{
		Enforcer:           enf,
		adapter:            adapter,
		clientset:          clientset,
		namespace:          namespace,
		configmap:          configmap,
		model:              builtInModel,
		claimsEnforcerFunc: claimsEnforcer,
	}
}

// SetDefaultRole sets a default role to use during enforcement. Will fall back to this role if
// normal enforcement fails
func (e *Enforcer) SetDefaultRole(roleName string) {
	e.defaultRole = roleName
}

// SetClaimsEnforcerFunc sets a claims enforce function during enforcement. The claims enforce function
// can extract claims from JWT token and do the proper enforcement based on user, group or any information
// available in the input parameter list
func (e *Enforcer) SetClaimsEnforcerFunc(claimsEnforcer ClaimsEnforcerFunc) {
	e.claimsEnforcerFunc = claimsEnforcer
}

// Enforce is a wrapper around casbin.Enforce to additionally enforce a default role and a custom
// claims function
func (e *Enforcer) Enforce(rvals ...interface{}) bool {
	return enforce(e.Enforcer, e.defaultRole, e.claimsEnforcerFunc, rvals...)
}

// EnforceRuntimePolicy enforces a policy defined at run-time which augments the built-in and
// user-defined policy. This allows any explicit denies of the built-in, and user-defined policies
// to override the run-time policy. Runs normal enforcement if run-time policy is empty.
func (e *Enforcer) EnforceRuntimePolicy(policy string, rvals ...interface{}) bool {
	var enf *casbin.Enforcer
	var err error
	if policy == "" {
		enf = e.Enforcer
	} else {
		policies := fmt.Sprintf("%s\n%s\n%s", e.builtinPolicy, e.userDefinedPolicy, policy)
		adapter := scas.NewAdapter(policies)
		enf, err = casbin.NewEnforcerSafe(newBuiltInModel(), adapter)
		if err != nil {
			log.Warnf("invalid runtime policy: %s", policy)
			enf = e.Enforcer
		}
	}
	return enforce(enf, e.defaultRole, e.claimsEnforcerFunc, rvals...)
}

// enforce is a helper to additionally check a default role and invoke a custom claims enforcement function
func enforce(enf *casbin.Enforcer, defaultRole string, claimsEnforcerFunc ClaimsEnforcerFunc, rvals ...interface{}) bool {
	// check the default role
	if defaultRole != "" && len(rvals) >= 2 {
		if enf.Enforce(append([]interface{}{defaultRole}, rvals[1:]...)...) {
			return true
		}
	}
	// check if subject is jwt.Claims vs. a normal subject string and run custom claims
	// enforcement func (if set)
	sub := rvals[0]
	switch sub.(type) {
	case string:
		// noop
	case jwt.Claims:
		if claimsEnforcerFunc != nil && claimsEnforcerFunc(sub.(jwt.Claims), rvals...) {
			return true
		}
		rvals = append([]interface{}{""}, rvals[1:]...)
	default:
		rvals = append([]interface{}{""}, rvals[1:]...)
	}
	return enf.Enforce(rvals...)
}

// SetBuiltinPolicy sets a built-in policy, which augments any user defined policies
func (e *Enforcer) SetBuiltinPolicy(policy string) error {
	e.builtinPolicy = policy
	e.adapter.Line = fmt.Sprintf("%s\n%s", e.builtinPolicy, e.userDefinedPolicy)
	return e.LoadPolicy()
}

// SetUserPolicy sets a user policy, augmenting the built-in policy
func (e *Enforcer) SetUserPolicy(policy string) error {
	e.userDefinedPolicy = policy
	e.adapter.Line = fmt.Sprintf("%s\n%s", e.builtinPolicy, e.userDefinedPolicy)
	return e.LoadPolicy()
}

// newInformers returns an informer which watches updates on the rbac configmap
func (e *Enforcer) newInformer() cache.SharedIndexInformer {
	tweakConfigMap := func(options *metav1.ListOptions) {
		cmFieldSelector := fields.ParseSelectorOrDie(fmt.Sprintf("metadata.name=%s", e.configmap))
		options.FieldSelector = cmFieldSelector.String()
	}
	return v1.NewFilteredConfigMapInformer(e.clientset, e.namespace, defaultRBACSyncPeriod, cache.Indexers{}, tweakConfigMap)
}

// RunPolicyLoader runs the policy loader which watches policy updates from the configmap and reloads them
func (e *Enforcer) RunPolicyLoader(ctx context.Context) error {
	cm, err := e.clientset.CoreV1().ConfigMaps(e.namespace).Get(e.configmap, metav1.GetOptions{})
	if err != nil {
		if !apierr.IsNotFound(err) {
			return err
		}
	} else {
		err = e.syncUpdate(cm)
		if err != nil {
			return err
		}
	}
	e.runInformer(ctx)
	return nil
}

func (e *Enforcer) runInformer(ctx context.Context) {
	cmInformer := e.newInformer()
	cmInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if cm, ok := obj.(*apiv1.ConfigMap); ok {
					err := e.syncUpdate(cm)
					if err != nil {
						log.Error(err)
					} else {
						log.Infof("RBAC ConfigMap '%s' added", e.configmap)
					}
				}
			},
			UpdateFunc: func(old, new interface{}) {
				oldCM := old.(*apiv1.ConfigMap)
				newCM := new.(*apiv1.ConfigMap)
				if oldCM.ResourceVersion == newCM.ResourceVersion {
					return
				}
				err := e.syncUpdate(newCM)
				if err != nil {
					log.Error(err)
				} else {
					log.Infof("RBAC ConfigMap '%s' updated", e.configmap)
				}
			},
		},
	)
	log.Info("Starting rbac config informer")
	cmInformer.Run(ctx.Done())
	log.Info("rbac configmap informer cancelled")
}

// syncUpdate updates the enforcer
func (e *Enforcer) syncUpdate(cm *apiv1.ConfigMap) error {
	e.SetDefaultRole(cm.Data[ConfigMapPolicyDefaultKey])
	policyCSV, ok := cm.Data[ConfigMapPolicyCSVKey]
	if !ok {
		policyCSV = ""
	}
	return e.SetUserPolicy(policyCSV)
}

// ValidatePolicy verifies a policy string is acceptable to casbin
func ValidatePolicy(policy string) error {
	adapter := scas.NewAdapter(policy)
	_, err := casbin.NewEnforcerSafe(newBuiltInModel(), adapter)
	if err != nil {
		return fmt.Errorf("policy syntax error: %s", policy)
	}
	return nil
}

// newBuiltInModel is a helper to return a brand new casbin model from the built-in model string.
// This is needed because it is not safe to re-use the same casbin Model when instantiating new
// casbin enforcers.
func newBuiltInModel() model.Model {
	return casbin.NewModel(modelConf)
}
