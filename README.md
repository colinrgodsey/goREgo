# **Go, RE, Go\!**

*"Conceived in a [Blaze](https://en.wikipedia.org/wiki/Bazel_(software)#History), born unto thy cluster."*

**goREgo** is a simplified, high-performance [Remote Execution](https://bazel.build/remote/rbe) implementation for Bazel (and other compatible build systems), built to strip away the complexity of existing solutions. It is designed to be lightweight, flexible, and ready to deploy—whether you are running on a single desktop or utilizing spare [Kubernetes](https://kubernetes.io/) resources.

## **🚀 Deployment Modes**

### *1\. go, RE, go\!* (The Local Hero)

Simple, single-node execution.  
Perfect for local testing or simple setups. Run it directly via bazel run or deploy using the default Helm single-node configuration. No external dependencies required.

```shell
$ bazel run //cmd/gorego
```

### *2\. go, REs, go\!* (The Clustered Beast)

Full Kubernetes cluster with backing cache.  
Why reinvent the wheel? We believe in delegating storage to the experts. This mode connects your execution cluster to an authoritative backing cache ([bazel-remote](https://github.com/buchgr/bazel-remote), [Google Cloud Storage](https://bazel.build/remote/caching#cloud-storage), [buildbuddy](https://www.buildbuddy.io/), and more).

```shell
$ helm install gorego ./charts/gorego/
```

### *3\. go, REs, go differently\!* (The Power User)

Total control.  
Full customization of the deployment via Helm and custom deploy containers.

## **🏗 Architecture**

**Making a complicated thing simple.**

Existing Remote Execution solutions are often heavy and tightly coupled. **goREgo** decouples the complexity by focusing on one thing: **Execution.**

* **Three Run Modes:**
  * Single Authoritative (all-in-one).
  * Multi-Node (authoritative independent cache w/ local cache).
  * Multi-Node (authoritative independent cache w/ just-in-time hydration).

### **Storage Delegation (CAS/AC)**

Content Addressable Storage (CAS) and Action Cache (AC) technology has already been perfected. We don't try to beat it; we use it.

* **goREgo** delegates persistence to an authoritative backing cache.
* Execution nodes remain stateless (mostly), keeping only an optional local hot-cache for performance.
* **Result:** Zero concerns about data consistency within the execution tier.

### **Distribution & Work Sharing**

How do we handle the load?

* **Active Work Offloading (Push Model):** When a node becomes overloaded, it doesn't wait for help: it acts.
* **Intelligent Routing:** Nodes utilize a lightweight gossip protocol (powered by [memberlist](https://github.com/hashicorp/memberlist)) to track the queue depth of their peers in real-time.
* **The Mechanism:** Using this gossip data, a busy node identifies underutilized peers and actively **pushes** tasks to them, ensuring the cluster stays balanced without heavy centralized scheduling.

## **🤔 "But clustering sounds hard..."**

Good news: It's not necessary.  
You can run **goREgo** locally as a single execution node. You don't even need the backing cache. This is perfect for the "Road Warrior" scenario where you just want to tap into your home build machine for fast builds.

# **💡 Inspiration**

Standing on the shoulders of giants:

* **[Buildfarm](https://github.com/buildfarm/buildfarm) & [Buildbarn](https://github.com/buildbarn):** For paving the way and getting me started on my RE journey.
* **[Just Execute](https://github.com/just-buildsystem/justbuild/blob/master/doc/tutorial/just-execute.org):** For the philosophy of simplicity.
* **My Utility Room:** From a burning desire to abuse the old rackmount servers gathering dust in my basement.

### *... was this vibe coded?*
The only vibe evoked was frustration. But Gemini was used for the initial research, implementation planning, and sub-system integration (with much human intervention, debugging, and review along the way). 
This project was born from a high-level architecture and a set of requirements I gathered while exploring different Remote Execution
solutions and fighting some of their complexity. Due to a real need for this solution and a lack of time, much of the work 
was delegated to AI agents.