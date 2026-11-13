#include "audio.h"

#include <dlfcn.h>
#include <string.h>
#include <unistd.h>

#define CHUNK 960
#define NUM_BUFFERS 16
#define AIPC_GID 2901

typedef void *(*open_play_fn)(int rate, int channels, int bits, const char *meta);
typedef void *(*write_buf_fn)(void *trans, int *status, unsigned timeout_ms,
                              unsigned long long timestamp);
typedef int (*commit_fn)(void *trans, unsigned bytes);
typedef unsigned (*count_fn)(void *trans);
typedef int (*close_fn)(void *handle);

static open_play_fn open_play;
static write_buf_fn write_buf;
static commit_fn commit;
static count_fn ready_blocks;
static count_fn block_bytes;
static close_fn close_play;

static int held[NUM_BUFFERS];
static int slot;
static int frame_bytes;
static void *player;

static int load(void) {
	if (close_play != NULL) return 0;
	void *lib = dlopen("libmixerAPI.so", RTLD_NOW);
	if (lib == NULL) return -1;
	open_play = (open_play_fn)dlsym(lib, "MixerOpenPlay");
	write_buf = (write_buf_fn)dlsym(lib, "_ZN5mixer9DataTrans12GetWriteBuffERijy");
	commit = (commit_fn)dlsym(lib, "_ZN5mixer9DataTrans15CommitWriteBuffEj");
	ready_blocks = (count_fn)dlsym(lib, "_ZN5mixer9DataTrans14GetNumBlkReadyEv");
	block_bytes = (count_fn)dlsym(lib, "_ZN5mixer9DataTrans14GetUserBlkSizeEv");
	close_fn closer = (close_fn)dlsym(lib, "MixerClose");
	if (open_play == NULL || write_buf == NULL || commit == NULL || ready_blocks == NULL ||
	    block_bytes == NULL || closer == NULL) {
		return -1;
	}
	close_play = closer;
	return 0;
}

int audio_open(int rate, int channels) {
	if (rate < 8000 || rate > 48000 || channels < 1 || channels > 2) return -1;
	if (load() != 0) return -1;

	frame_bytes = channels * 2;
	slot = 0;
	memset(held, 0, sizeof(held));

	gid_t gid = getegid();
	if (setegid(AIPC_GID) != 0) return -1;
	player = open_play(rate, channels, 16, "Music");
	if (setegid(gid) != 0 || player == NULL) {
		audio_close();
		return -1;
	}
	if (block_bytes(player) < CHUNK) {
		audio_close();
		return -1;
	}
	return 0;
}

int audio_write(const unsigned char *pcm, size_t len) {
	if (player == NULL) return -1;
	if (ready_blocks(player) >= NUM_BUFFERS) return 0;

	size_t n = len > CHUNK ? CHUNK : len;
	n -= n % (size_t)frame_bytes;
	if (n == 0) return -1;

	int status = 0;
	unsigned char *buf = write_buf(player, &status, 0, 0);
	if (buf == NULL) return status == 11 ? 0 : -1;
	memcpy(buf, pcm, n);
	commit(player, (unsigned)n);
	held[slot] = (int)(n / (size_t)frame_bytes);
	slot = (slot + 1) % NUM_BUFFERS;
	return (int)n;
}

long long audio_pending(void) {
	if (player == NULL) return -1;

	unsigned count = ready_blocks(player);
	if (count > NUM_BUFFERS) return -1;

	long long frames = 0;
	for (unsigned i = 1; i <= count; i++) {
		frames += held[(slot + NUM_BUFFERS - (int)i) % NUM_BUFFERS];
	}
	return frames;
}

int audio_start(void) { return player == NULL ? -1 : 0; }

void audio_close(void) {
	if (player != NULL) {
		close_play(player);
		player = NULL;
	}
	slot = 0;
	memset(held, 0, sizeof(held));
}
