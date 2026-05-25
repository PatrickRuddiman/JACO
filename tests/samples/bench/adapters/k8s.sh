#!/usr/bin/env bash
# Kubernetes (kubeadm) adapter — STUB. Implemented in the follow-up pass.
# Will: kubeadm init on node-1, join node-2/3, install a CNI + ingress
# controller, apply ../../k8s/manifests, and expose web behind the LB on 80/443.

adapter_label() { echo "Kubernetes (kubeadm)"; }
adapter_deploy()   { die "k8s adapter not implemented yet (see samples/k8s/README.md)"; }
adapter_collect()  { :; }
adapter_teardown() { :; }
