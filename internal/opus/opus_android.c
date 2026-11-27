#include "opus.h"

#include <dlfcn.h>
#include <pthread.h>
#include <stddef.h>

typedef void *(*create_fn)(int rate, int channels, int *error);
typedef int (*decode_fn)(void *st, const unsigned char *data, int len, short *pcm,
                         int frames, int fec);
typedef void (*destroy_fn)(void *st);

static create_fn create;
static decode_fn decode;
static destroy_fn destroy;

static pthread_once_t once = PTHREAD_ONCE_INIT;

static void load_once(void) {
	void *lib = dlopen("libopus.so", RTLD_NOW);
	if (lib == NULL) return;
	create = (create_fn)dlsym(lib, "opus_decoder_create");
	decode = (decode_fn)dlsym(lib, "opus_decode");
	destroy = (destroy_fn)dlsym(lib, "opus_decoder_destroy");
}

static int load(void) {
	pthread_once(&once, load_once);
	return create != NULL && decode != NULL && destroy != NULL ? 0 : -1;
}

void *overdub_opus_new(int rate, int channels) {
	if (load() != 0) return NULL;
	int error = 0;
	void *st = create(rate, channels, &error);
	if (error != 0) return NULL;
	return st;
}

int overdub_opus_decode(void *st, const unsigned char *data, int len, short *pcm, int frames) {
	return decode(st, data, len, pcm, frames, 0);
}

void overdub_opus_free(void *st) {
	if (st != NULL) destroy(st);
}
