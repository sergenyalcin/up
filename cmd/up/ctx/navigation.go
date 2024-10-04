// Copyright 2024 Upbound Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ctx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spacesv1beta1 "github.com/upbound/up-sdk-go/apis/spaces/v1beta1"
	upboundv1alpha1 "github.com/upbound/up-sdk-go/apis/upbound/v1alpha1"
	"github.com/upbound/up-sdk-go/service/organizations"
	"github.com/upbound/up/internal/profile"
	"github.com/upbound/up/internal/spaces"
	"github.com/upbound/up/internal/upbound"
	"github.com/upbound/up/internal/version"
)

var (
	upboundBrandColor = lipgloss.AdaptiveColor{Light: "#5e3ba5", Dark: "#af7efd"}
	neutralColor      = lipgloss.AdaptiveColor{Light: "#4e5165", Dark: "#9a9ca7"}
	dimColor          = lipgloss.AdaptiveColor{Light: "#9B9B9B", Dark: "#5C5C5C"}
)

var (
	// all adaptive colors have a minimum of 7:1 against #fff or #000
	upboundRootStyle         = lipgloss.NewStyle().Foreground(upboundBrandColor)
	pathInactiveSegmentStyle = lipgloss.NewStyle().Foreground(neutralColor)
	pathSegmentStyle         = lipgloss.NewStyle()
)

// NavigationState is a model state that provides a list of items for a navigation node.
type NavigationState interface {
	Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error)
	Breadcrumbs() string
}

// Accepting is a model state that provides a method to accept a navigation node.
type Accepting interface {
	NavigationState
	Accept(upCtx *upbound.Context, navCtx *navContext) (string, error)
}

// Back is a model state that provides a method to go back to the parent navigation node.
type Back interface {
	NavigationState
	Back(m model) (model, error)
	BackLabel() string
}

type AcceptingFunc func(ctx context.Context, upCtx *upbound.Context) error

func (f AcceptingFunc) Accept(ctx context.Context, upCtx *upbound.Context) error {
	return f(ctx, upCtx)
}

// breadcrumbStyle defines the styles to be used in the breadcrumbs of a list
type breadcrumbStyle struct {
	// previousLevel is the style of the previous levels in the path (higher
	// order items). For example, when listing control planes then the
	// breadcrumb labels for groups, spaces, orgs and root will be rendered with
	// this style.
	previousLevel lipgloss.Style

	// currentLevel is the style of the current level in the path. For example,
	// when listing control planes then the breadcrumb label for control planes
	// will be rendered with this style.
	currentLevel lipgloss.Style
}

var defaultBreadcrumbStyle = breadcrumbStyle{
	previousLevel: pathInactiveSegmentStyle,
	currentLevel:  pathSegmentStyle,
}

type Root struct{}

func (r *Root) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) {
	cfg, err := upCtx.BuildSDKConfig()
	if err != nil {
		return nil, err
	}

	client := organizations.NewClient(cfg)

	items := make([]list.Item, 0, 1)

	orgs, err := client.List(ctx)
	if err != nil {
		// We want `up ctx` to be usable for disconnected spaces even if the
		// user isn't logged in or can't connect to Upbound. Return a friendly
		// message instead of an error.
		items = append(items, item{ //nolint:nilerr
			text:          "Could not list Upbound organizations; are you logged in?",
			notSelectable: true,
		})
	}

	for _, org := range orgs {
		items = append(items, item{text: org.DisplayName, kind: "organization", matchingTerms: []string{org.Name}, onEnter: func(m model) (model, error) {
			m.state = &Organization{Name: org.Name}
			return m, nil
		}})
	}

	sort.Sort(sortedItems(items))
	return append(items, item{
		text: "Disconnected Spaces",
		onEnter: func(m model) (model, error) {
			m.state = &Disconnected{}
			return m, nil
		},
		padding: padding{
			top: 1,
		},
		matchingTerms: []string{"disconnected"},
	}), nil
}

func (r *Root) Breadcrumbs() string {
	return ""
}

type Disconnected struct{}

func (d *Disconnected) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) {
	kubeconfig, err := upCtx.Kubecfg.RawConfig()
	if err != nil {
		return nil, err
	}

	items := make([]list.Item, 0, 1)
	items = append(items, item{text: "..", kind: d.BackLabel(), onEnter: d.Back, back: true})

	var wg sync.WaitGroup
	var mu sync.Mutex
	for name := range kubeconfig.Contexts {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			itm, err := spaceItemFromKubeContext(ctx, kubeconfig, name)
			if err != nil || itm == nil {
				// Context is not a Space, or we can't tell due to an error.
				return
			}

			mu.Lock()
			items = append(items, itm)
			mu.Unlock()
		}(name)
	}
	wg.Wait()

	sort.Sort(sortedItems(items))
	return items, nil
}

func spaceItemFromKubeContext(ctx context.Context, kubeconfig clientcmdapi.Config, ctxName string) (list.Item, error) {
	kubectx := kubeconfig.Contexts[ctxName]
	spacesExt, err := upbound.GetSpaceExtension(kubectx)
	if err != nil {
		return nil, err
	}
	if spacesExt != nil {
		// This is an up-managed context, which means it's either a cloud
		// Space, or a disconnected Space represented by some other
		// kubeconfig context, which we'll find later.
		return nil, nil
	}

	// If the context points at a Space, it will have a ConfigMap containing
	// the Space's ingress information. If we can't fetch the ConfigMap for
	// any reason, assume the context isn't a Space.

	rest, err := clientcmd.NewDefaultClientConfig(kubeconfig, &clientcmd.ConfigOverrides{
		CurrentContext: ctxName,
	}).ClientConfig()
	if err != nil {
		return nil, err
	}

	cl, err := corev1client.NewForConfig(rest)
	if err != nil {
		return nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ingressHost, ingressCA, err := profile.GetIngressHost(reqCtx, cl)
	if err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	return item{text: ctxName, kind: "space", onEnter: func(m model) (model, error) {
		m.state = &Space{
			Name: ctxName,
			Ingress: spaces.SpaceIngress{
				Host:   ingressHost,
				CAData: ingressCA,
			},
			HubContext: ctxName,
		}
		return m, nil
	}}, nil
}

func (d *Disconnected) breadcrumbs(styles breadcrumbStyle) string {
	return styles.currentLevel.Render("disconnected/")
}

func (d *Disconnected) Breadcrumbs() string {
	return d.breadcrumbs(defaultBreadcrumbStyle)
}

func (d *Disconnected) Back(m model) (model, error) {
	m.state = &Root{}
	return m, nil
}

func (d *Disconnected) BackLabel() string {
	return "home"
}

var _ Back = &Organization{}

type Organization struct {
	Name string
}

func (o *Organization) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) { //nolint:gocyclo
	cloudCfg, err := upCtx.BuildControllerClientConfig()
	if err != nil {
		return nil, err
	}

	cloudClient, err := client.New(cloudCfg, client.Options{})
	if err != nil {
		return nil, err
	}

	var l upboundv1alpha1.SpaceList
	err = cloudClient.List(ctx, &l, &client.ListOptions{Namespace: o.Name})
	if err != nil {
		return nil, err
	}

	authInfo, err := getOrgScopedAuthInfo(upCtx, o.Name)
	if err != nil {
		return nil, err
	}

	// Find ingresses for up to 20 Spaces in parallel to construct items for the
	// list.
	var wg sync.WaitGroup
	var mu sync.Mutex
	items := make([]list.Item, 0)
	unselectableItems := make([]list.Item, 0)
	ch := make(chan upboundv1alpha1.Space, len(l.Items))
	for i := 0; i < min(20, len(l.Items)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for space := range ch {
				if mode, ok := space.ObjectMeta.Labels[upboundv1alpha1.SpaceModeLabelKey]; ok {
					if mode == string(upboundv1alpha1.ModeLegacy) {
						continue
					}
				}

				if space.Status.ConnectionDetails.Status == upboundv1alpha1.ConnectionStatusUnreachable {
					mu.Lock()
					unselectableItems = append(unselectableItems, item{
						text:          space.GetObjectMeta().GetName() + " (unreachable)",
						kind:          "space",
						notSelectable: true,
					})
					mu.Unlock()
					continue
				}

				ingress, err := navCtx.ingressReader.Get(ctx, space)
				if err != nil {
					mu.Lock()
					if errors.Is(err, spaces.SpaceConnectionError) {
						unselectableItems = append(unselectableItems, item{
							text:          space.GetObjectMeta().GetName() + " (unreachable)",
							kind:          "space",
							notSelectable: true,
						})
					} else {
						unselectableItems = append(unselectableItems, item{
							text:          fmt.Sprintf("%s (error: %v)", space.GetObjectMeta().GetName(), err),
							kind:          "space",
							notSelectable: true,
						})
					}
					mu.Unlock()
					continue
				}

				mu.Lock()
				items = append(items, item{text: space.GetObjectMeta().GetName(), kind: "space", onEnter: func(m model) (model, error) {
					m.state = &Space{
						Org:      *o,
						Name:     space.GetObjectMeta().GetName(),
						Ingress:  *ingress,
						AuthInfo: authInfo,
					}
					return m, nil
				}})
				mu.Unlock()
			}
		}()
	}
	for _, space := range l.Items {
		ch <- space
	}
	close(ch)
	wg.Wait()

	sort.Sort(sortedItems(items))
	sort.Sort(sortedItems(unselectableItems))

	ret := []list.Item{item{text: "..", kind: o.BackLabel(), onEnter: o.Back, back: true}}
	ret = append(ret, items...)
	ret = append(ret, unselectableItems...)
	return ret, nil
}

func (o *Organization) Back(m model) (model, error) {
	m.state = &Root{}
	return m, nil
}

func (o *Organization) BackLabel() string {
	return "home"
}

func (o *Organization) breadcrumbs(styles breadcrumbStyle) string {
	return styles.currentLevel.Render(fmt.Sprintf("%s/", o.Name))
}

func (o *Organization) Breadcrumbs() string {
	return o.breadcrumbs(defaultBreadcrumbStyle)
}

type sortedItems []list.Item

func (s sortedItems) Len() int           { return len(s) }
func (s sortedItems) Less(i, j int) bool { return s[i].(item).text < s[j].(item).text }
func (s sortedItems) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

var _ Back = &Space{}

// Space provides the navigation node for a space.
type Space struct {
	Org  Organization
	Name string

	Ingress  spaces.SpaceIngress
	AuthInfo *clientcmdapi.AuthInfo

	// HubContext is an optional field that stores which context in the
	// kubeconfig points at the hub
	HubContext string
}

func (s *Space) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) {
	cl, err := s.GetClient(upCtx)
	if err != nil {
		return nil, err
	}

	nss := &corev1.NamespaceList{}
	if err := cl.List(ctx, nss, client.MatchingLabels(map[string]string{spacesv1beta1.ControlPlaneGroupLabelKey: "true"})); err != nil {
		return nil, err
	}

	items := make([]list.Item, 0, len(nss.Items)+3)
	items = append(items, item{text: "..", kind: s.BackLabel(), onEnter: s.Back, back: true})
	for _, ns := range nss.Items {
		items = append(items, item{text: ns.Name, kind: "group", onEnter: func(m model) (model, error) {
			m.state = &Group{Space: *s, Name: ns.Name}
			return m, nil
		}})
	}

	if len(nss.Items) == 0 {
		items = append(items, item{text: "No groups found", notSelectable: true})
	}

	items = append(items, item{text: fmt.Sprintf("Switch context to %q", s.Name), onEnter: func(m model) (model, error) {
		msg, err := s.Accept(m.upCtx, m.navContext)
		if err != nil {
			return m, err
		}
		return m.WithTermination(msg, nil), nil
	}})

	return items, nil
}

func (s *Space) Back(m model) (model, error) {
	if s.IsCloud() {
		m.state = &s.Org
	} else {
		m.state = &Disconnected{}
	}
	return m, nil
}

func (s *Space) BackLabel() string {
	return "spaces"
}

func (s *Space) IsCloud() bool {
	return s.Org.Name != ""
}

func (s *Space) breadcrumbs(styles breadcrumbStyle) string {
	if s.IsCloud() {
		return s.Org.breadcrumbs(breadcrumbStyle{
			currentLevel:  styles.previousLevel,
			previousLevel: styles.previousLevel,
		}) + styles.currentLevel.Render(fmt.Sprintf("%s/", s.Name))
	} else {
		return (&Disconnected{}).breadcrumbs(breadcrumbStyle{
			currentLevel:  styles.previousLevel,
			previousLevel: styles.previousLevel,
		}) + styles.currentLevel.Render(fmt.Sprintf("%s/", s.Name))
	}
}

func (s *Space) Breadcrumbs() string {
	return s.breadcrumbs(defaultBreadcrumbStyle)
}

// GetClient returns a kube client pointed at the current space
func (s *Space) GetClient(upCtx *upbound.Context) (client.Client, error) {
	conf, err := s.buildClient(upCtx, types.NamespacedName{})
	if err != nil {
		return nil, err
	}

	rest, err := conf.ClientConfig()
	if err != nil {
		return nil, err
	}
	rest.UserAgent = version.UserAgent()

	return client.New(rest, client.Options{})
}

// buildSpacesClient creates a new kubeconfig hardcoded to match the provided
// spaces access configuration and pointed directly at the resource. If the
// resource only specifies a namespace, then the client will point at the space
// and the context will be set at the group. If the resource specifies both a
// namespace and a name, then the client will point directly at the control
// plane ingress and set the namespace to "default".
func (s *Space) buildClient(upCtx *upbound.Context, resource types.NamespacedName) (clientcmd.ClientConfig, error) {
	// reference name for all context, cluster and authinfo for in-memory
	// kubeconfig
	ref := "upbound"

	prev, err := upCtx.Kubecfg.RawConfig()
	if err != nil {
		return nil, err
	}

	config := clientcmdapi.Config{
		Kind:           "Config",
		APIVersion:     "v1",
		CurrentContext: ref,
		Clusters:       make(map[string]*clientcmdapi.Cluster),
		Contexts:       make(map[string]*clientcmdapi.Context),
		AuthInfos:      make(map[string]*clientcmdapi.AuthInfo),
	}

	// Build a new context with a new cluster that points to the space's
	// ingress.
	refContext := &clientcmdapi.Context{
		Extensions: make(map[string]runtime.Object),
		Cluster:    ref,
	}

	if s.Ingress.Host == "" {
		return nil, errors.New("missing ingress address for context")
	}
	if len(s.Ingress.CAData) == 0 {
		return nil, errors.New("missing ingress CA for context")
	}

	config.Clusters[ref] = &clientcmdapi.Cluster{
		Server:                   profile.ToSpacesK8sURL(s.Ingress.Host, resource),
		CertificateAuthorityData: s.Ingress.CAData,
	}

	// Use the space's authinfo if we have it, otherwise fall back to the hub
	// context's auth.
	switch {
	case s.AuthInfo != nil:
		config.AuthInfos[ref] = s.AuthInfo
		refContext.AuthInfo = ref
	case s.HubContext != "":
		hubContext, ok := prev.Contexts[s.HubContext]
		if ok {
			// import the authinfo from the hub context
			refContext.AuthInfo = hubContext.AuthInfo
			config.AuthInfos[hubContext.AuthInfo] = ptr.To(*prev.AuthInfos[hubContext.AuthInfo])
		}
	default:
		return nil, errors.New("no auth info for context")
	}

	if resource.Name == "" {
		// point at the relevant namespace in the space hub
		refContext.Namespace = resource.Namespace
	} else {
		// since we are pointing at an individual control plane, point at the
		// "default" namespace inside it
		refContext.Namespace = "default"
	}

	if s.IsCloud() {
		refContext.Extensions[upbound.ContextExtensionKeySpace] = upbound.NewCloudV1Alpha1SpaceExtension(s.Org.Name, s.Name)
	} else {
		refContext.Extensions[upbound.ContextExtensionKeySpace] = upbound.NewDisconnectedV1Alpha1SpaceExtension(s.HubContext)
	}

	config.Contexts[ref] = refContext
	return clientcmd.NewDefaultClientConfig(config, &clientcmd.ConfigOverrides{}), nil
}

// Group provides the navigation node for a concrete group aka namespace.
type Group struct {
	Space Space
	Name  string
}

var _ Accepting = &Group{}
var _ Back = &Group{}

func (g *Group) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) {
	cl, err := g.Space.GetClient(upCtx)
	if err != nil {
		return nil, err
	}

	ctps := &spacesv1beta1.ControlPlaneList{}
	if err := cl.List(ctx, ctps, client.InNamespace(g.Name)); err != nil {
		return nil, err
	}

	items := make([]list.Item, 0, len(ctps.Items)+3)
	items = append(items, item{text: "..", kind: g.BackLabel(), onEnter: g.Back, back: true})

	for _, ctp := range ctps.Items {
		items = append(items, item{text: ctp.Name, kind: "controlplane", onEnter: func(m model) (model, error) {
			m.state = &ControlPlane{Group: *g, Name: ctp.Name}
			return m, nil
		}})
	}

	if len(ctps.Items) == 0 {
		items = append(items, item{text: fmt.Sprintf("No control planes found in group %q", g.Name), notSelectable: true})
	}

	items = append(items, item{text: fmt.Sprintf("Switch context to %q", fmt.Sprintf("%s/%s", g.Space.Name, g.Name)), onEnter: func(m model) (model, error) {
		msg, err := g.Accept(m.upCtx, m.navContext)
		if err != nil {
			return m, err
		}
		return m.WithTermination(msg, nil), nil
	}})

	return items, nil
}

func (g *Group) breadcrumbs(styles breadcrumbStyle) string {
	return g.Space.breadcrumbs(breadcrumbStyle{
		currentLevel:  styles.previousLevel,
		previousLevel: styles.previousLevel,
	}) + styles.currentLevel.Render(fmt.Sprintf("%s/", g.Name))
}

func (g *Group) Breadcrumbs() string {
	return g.breadcrumbs(defaultBreadcrumbStyle)
}

func (g *Group) Back(m model) (model, error) {
	m.state = &g.Space
	return m, nil
}

func (g *Group) BackLabel() string {
	return "groups"
}

// ControlPlane provides the navigation node for a concrete controlplane.
type ControlPlane struct {
	Group Group
	Name  string
}

var _ Accepting = &ControlPlane{}
var _ Back = &ControlPlane{}

func (ctp *ControlPlane) Items(ctx context.Context, upCtx *upbound.Context, navCtx *navContext) ([]list.Item, error) {
	return []list.Item{
		item{text: "..", kind: ctp.BackLabel(), onEnter: ctp.Back, back: true},
		item{text: fmt.Sprintf("Connect to %q and quit", ctp.NamespacedName().Name), onEnter: KeyFunc(func(m model) (model, error) {
			msg, err := ctp.Accept(m.upCtx, m.navContext)
			if err != nil {
				return m, err
			}
			return m.WithTermination(msg, nil), nil
		})},
	}, nil
}

func (ctp *ControlPlane) breadcrumbs(styles breadcrumbStyle) string {
	// use current level to highlight the entire breadcrumb chain
	return ctp.Group.breadcrumbs(breadcrumbStyle{
		currentLevel:  styles.previousLevel,
		previousLevel: styles.previousLevel,
	}) + styles.currentLevel.Render(ctp.Name)
}

func (ctp *ControlPlane) Breadcrumbs() string {
	return ctp.breadcrumbs(defaultBreadcrumbStyle)
}

func (ctp *ControlPlane) Back(m model) (model, error) {
	m.state = &ctp.Group
	return m, nil
}

func (ctp *ControlPlane) BackLabel() string {
	return "controlplanes"
}

func (ctp *ControlPlane) NamespacedName() types.NamespacedName {
	return types.NamespacedName{Name: ctp.Name, Namespace: ctp.Group.Name}
}

func getOrgScopedAuthInfo(upCtx *upbound.Context, orgName string) (*clientcmdapi.AuthInfo, error) {
	// find the current executable path
	cmd, err := os.Executable()
	if err != nil {
		return nil, err
	}

	// if the current executable was the same `up` that is found in PATH
	path, err := exec.LookPath("up")
	if err == nil && path == cmd {
		cmd = "up"
	}

	return &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1",
			Command:    cmd,
			Args:       []string{"organization", "token"},
			Env: []clientcmdapi.ExecEnvVar{
				{
					Name:  "ORGANIZATION",
					Value: orgName,
				},
				{
					Name:  "UP_PROFILE",
					Value: upCtx.ProfileName,
				},
			},
			InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
		},
	}, nil
}
