#include <iostream>
#include <boost/filesystem.hpp>
#include <boost/algorithm/string.hpp>
#include <map>
#include <list>
#include <sys/types.h>
#include <sys/stat.h>
#include <unistd.h>
extern "C" {
#include "hdparm.h"
}

/**
 * https://svn.boost.org/trac/boost/ticket/1976#comment:2
 * 
 * "The idea: uncomplete(/foo/new, /foo/bar) => ../new
 *  The use case for this is any time you get a full path (from an open dialog, perhaps)
 *  and want to store a relative path so that the group of files can be moved to a different
 *  directory without breaking the paths. An IDE would be a simple example, so that the
 *  project file could be safely checked out of subversion."
 * 
 * ALGORITHM:
 *  iterate path and base
 * compare all elements so far of path and base
 * whilst they are the same, no write to output
 * when they change, or one runs out:
 *   write to output, ../ times the number of remaining elements in base
 *   write to output, the remaining elements in path
 */
boost::filesystem::path
naive_uncomplete(boost::filesystem::path const p, boost::filesystem::path const base) {

    using boost::filesystem::path;

    if (p == base)
        return "./";
        /*!! this breaks stuff if path is a filename rather than a directory,
             which it most likely is... but then base shouldn't be a filename so... */

    boost::filesystem::path from_path, from_base, output;

    boost::filesystem::path::iterator path_it = p.begin(),    path_end = p.end();
    boost::filesystem::path::iterator base_it = base.begin(), base_end = base.end();

    // check for emptiness
    if ((path_it == path_end) || (base_it == base_end))
        throw std::runtime_error("path or base was empty; couldn't generate relative path");

    // Cache system-dependent dot, double-dot and slash strings
    const std::string _dot  = ".";
    const std::string _dots = "..";
    const std::string _sep = "/";

    // iterate over path and base
    while (true) {

        // compare all elements so far of path and base to find greatest common root;
        // when elements of path and base differ, or run out:
        if ((path_it == path_end) || (base_it == base_end) || (*path_it != *base_it)) {

            // write to output, ../ times the number of remaining elements in base;
            // this is how far we've had to come down the tree from base to get to the common root
            for (; base_it != base_end; ++base_it) {
                if (*base_it == _dot)
                    continue;
                else if (*base_it == _sep)
                    continue;

                output /= "../";
            }

            // write to output, the remaining elements in path;
            // this is the path relative from the common root
            boost::filesystem::path::iterator path_it_start = path_it;
            for (; path_it != path_end; ++path_it) {

                if (path_it != path_it_start)
                    output /= "/";

                if (*path_it == _dot)
                    continue;
                if (*path_it == _sep)
                    continue;

                output /= *path_it;
            }

            break;
        }

        // add directory level to both paths and continue iteration
        from_path /= path(*path_it);
        from_base /= path(*base_it);

        ++path_it, ++base_it;
    }

    return output;
}

int main(int argc, char **argv) {
  char linkbuf[2048];
  std::map<unsigned long long int,std::string> files;
  boost::filesystem::recursive_directory_iterator recit(argv[1], boost::filesystem::symlink_option::no_recurse);
  boost::filesystem::recursive_directory_iterator endit;
  std::list<std::string> emptyfiles;
  std::list<std::pair<std::string,std::string> > symlinks;
  std::list<std::pair<std::string,std::string> > hardlinks;
  std::cerr << "Enumerating files..." << std::endl;
  for (; recit != endit;)
  {
    boost::filesystem::directory_entry entry = (*recit);
    try {
      
      boost::filesystem::file_type t = entry.status().type();
      bool islink = false;
      struct stat buf;
      int ret = lstat(entry.path().string().c_str(),&buf);
      if ( ret == 0 )
	islink = S_ISLNK(buf.st_mode);
      if ( t == boost::filesystem::regular_file && !islink )
      {
	unsigned long long int lba = do_filemap(entry.path().string().c_str());
	if ( lba > 0 )
	{
	  if ( files.find(lba) != files.end() ) // Hard link
	  {
	    hardlinks.push_back(std::pair<std::string,std::string>(files[lba], entry.path().string()));
	    //std::cerr << "DUPLICATE LBA " << lba << "(" << entry.path().string() << "," << files[lba] << ")" << std::endl;
	    //abort();
	  }
	  files.insert(std::pair<unsigned long long int,std::string>(lba,naive_uncomplete(entry.path(),boost::filesystem::path(argv[1])).string()));
	}else{
	  emptyfiles.push_back(entry.path().string());
	}
	if ( (files.size()+emptyfiles.size()) % 1000 == 0 )
	  std::cerr << (files.size()+emptyfiles.size()) << "         \r";
	
      }
      if ( t == boost::filesystem::regular_file && islink )
      {

	readlink(entry.path().string().c_str(),linkbuf,2047);
	symlinks.push_back(std::pair<std::string,std::string>(entry.path().string().c_str(),linkbuf));
      }
      recit++;
    } catch ( boost::filesystem::filesystem_error e )
    {
      std::cerr << "Warning error accessing " << entry.path().string() << " :" << e.what() << std::endl;
      recit++;
    }
  }
  
  std::cout << symlinks.size() << " symbolic links, " << hardlinks.size() << " hardlinks, " << files.size() << " regular files" << std::endl;
  int i = 0;
  for ( std::map<unsigned long long int,std::string>::iterator it = files.begin(); it != files.end(); it++,i++ )
  {
    //std::cout << (*it).first << " " << (*it).second << std::endl;
    boost::filesystem::path a = boost::filesystem::absolute((*it).second,argv[2]);
    boost::filesystem::path s = boost::filesystem::absolute((*it).second,argv[1]);
    boost::filesystem::create_directories(a.parent_path());
    
    //std::cout << boost::filesystem::absolute((*it).second,argv[1]).string() << " -> " << boost::filesystem::absolute((*it).second,argv[2]).string() << std::endl; 
    if ( i % 100 == 0 )
    {
      std::string bars;
      for ( int x = 0; x < i/(files.size()/30); x++ )
	bars += "#";
      
      std::cout << "\r\033[31;1m[\033[34m";
      std::cout << bars;
      std::cout << "\033[31;1m]\033[0m " << ((float)i/float(files.size()))*100.0 << " %   ";
    }
    boost::filesystem::copy(s,a);
  }
  
  return 0;
}
