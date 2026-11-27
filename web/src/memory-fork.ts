import type { Capability, MemoryForkBackend, MemoryForkProfile } from "../../sdk/typescript/src/index"

export const kernelCowProfile: MemoryForkProfile = "kernel_cow_anon_v1"

const memoryForkBackendLabels: Record<MemoryForkBackend, string> = {
  native: "Native",
  kernel_cow: "Kernel CoW",
  auto: "Auto",
}

export function memoryForkBackendLabel(backend: MemoryForkBackend) {
  return memoryForkBackendLabels[backend] ?? backend
}

export function memoryForkProfileLabel(profile: string) {
  if (profile === kernelCowProfile) return "Kernel CoW (fixed anonymous RAM)"
  return profile === "native" ? memoryForkBackendLabels.native : profile
}

export function forkMemorySupport(capability?: Capability) {
  const available = capability?.status === "supported" && capability.supported
  const observation = available ? capability.memory_fork : undefined
  return {
    available,
    observation,
    native: available && (!observation || (observation.profile === "native" && observation.native_supported)),
    kernelCow: observation?.profile === kernelCowProfile && observation.kernel_cow_supported,
  }
}
